// Package upnp pushes a stream URL to the SoundTouch's own UPnP/DLNA AVTransport
// renderer (SetAVTransportURI + Play) — the same mechanism SoundTouch-Pi uses to
// play internet radio without the Bose cloud. Here the controller and renderer
// are the same device.
package upnp

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const avTransportType = "urn:schemas-upnp-org:service:AVTransport:1"

type service struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type device struct {
	ServiceList struct {
		Services []service `xml:"service"`
	} `xml:"serviceList"`
	DeviceList struct {
		Devices []device `xml:"device"`
	} `xml:"deviceList"`
}

type descRoot struct {
	URLBase string `xml:"URLBase"`
	Device  device `xml:"device"`
}

func collectServices(d device, out *[]service) {
	*out = append(*out, d.ServiceList.Services...)
	for _, sub := range d.DeviceList.Devices {
		collectServices(sub, out)
	}
}

// Player holds a resolved AVTransport control URL.
type Player struct {
	ControlURL string
	client     *http.Client
}

// FindControlURL locates the speaker's AVTransport control URL. It tries the
// known Bose description path derived from the deviceID first, then the
// description LOCATION the renderer advertises over SSDP.
func FindControlURL(host, deviceID string) (string, error) {
	return firstControlURL(candidates(host, deviceID, ssdpLocation), host)
}

// candidates lists the description URLs to try, in order. ssdp is injected so
// the ordering can be tested without multicast.
func candidates(host, deviceID string, ssdp func(host, deviceID string) (string, error)) []string {
	var locs []string
	// Prefer the deviceID-derived description: it is definitively THIS speaker
	// (the network may have other UPnP renderers — TVs, other speakers).
	if deviceID != "" {
		locs = append(locs, boseDescURL(host, deviceID))
	}
	if loc, err := ssdp(host, deviceID); err == nil && loc != "" {
		locs = append(locs, loc)
	}
	return locs
}

// boseDescURL is where SoundTouch firmware serves its UPnP device description.
func boseDescURL(host, deviceID string) string {
	return fmt.Sprintf("http://%s:8091/XD/BO5EBO5E-F00D-F00D-FEED-%s.xml",
		host, strings.ToUpper(deviceID))
}

// firstControlURL returns the AVTransport control URL from the first
// description in locs that has one.
func firstControlURL(locs []string, host string) (string, error) {
	var lastErr error
	for _, loc := range locs {
		ctrl, err := controlURLFromDesc(loc)
		if err == nil {
			return ctrl, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("AVTransport control URL not found for %s", host)
	}
	return "", lastErr
}

func controlURLFromDesc(descURL string) (string, error) {
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Get(descURL)
	if err != nil {
		return "", err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	var root descRoot
	if err := xml.Unmarshal(body, &root); err != nil {
		return "", err
	}
	var svcs []service
	collectServices(root.Device, &svcs)
	for _, s := range svcs {
		if strings.Contains(s.ServiceType, "AVTransport") && s.ControlURL != "" {
			base := root.URLBase
			if base == "" {
				base = descURL // resolve relative controlURL against the description URL
			}
			return resolveRef(base, s.ControlURL), nil
		}
	}
	return "", fmt.Errorf("no AVTransport service in %s", descURL)
}

// ssdpLocation sends an SSDP M-SEARCH for a MediaRenderer and returns the
// advertised description LOCATION. When deviceID is set, only a response whose
// USN/headers contain that id is accepted (avoids picking a foreign renderer).
func ssdpLocation(host, deviceID string) (string, error) {
	mcast, err := net.ResolveUDPAddr("udp4", "239.255.255.250:1900")
	if err != nil {
		return "", err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: 0})
	if err != nil {
		return "", err
	}
	defer conn.Close()

	msg := "M-SEARCH * HTTP/1.1\r\n" +
		"HOST: 239.255.255.250:1900\r\n" +
		"MAN: \"ssdp:discover\"\r\n" +
		"MX: 2\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"
	if _, err := conn.WriteToUDP([]byte(msg), mcast); err != nil {
		return "", err
	}

	_ = conn.SetReadDeadline(time.Now().Add(4 * time.Second))
	buf := make([]byte, 65535)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			return "", err
		}
		if loc, ok := matchSSDP(string(buf[:n]), from.IP, host, deviceID); ok {
			return loc, nil
		}
	}
}

// matchSSDP returns the LOCATION from an SSDP response if it came from the
// speaker we are looking for. Every MediaRenderer on the LAN answers the same
// M-SEARCH, so responses are filtered by source address (when host is remote)
// and by deviceID (when set).
func matchSSDP(payload string, from net.IP, host, deviceID string) (string, bool) {
	local := host == "" || host == "127.0.0.1" || host == "localhost"
	if !local && from.String() != host {
		return "", false
	}
	if wantID := strings.ToUpper(deviceID); wantID != "" && !strings.Contains(strings.ToUpper(payload), wantID) {
		return "", false // a different UPnP renderer on the network
	}
	for _, line := range strings.Split(payload, "\n") {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "location:") {
			return strings.TrimSpace(line[strings.Index(line, ":")+1:]), true
		}
	}
	return "", false
}

func resolveRef(base, ref string) string {
	b, err := url.Parse(base)
	if err != nil {
		return ref
	}
	r, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return b.ResolveReference(r).String()
}

// New returns a Player for the given AVTransport control URL.
func New(controlURL string) *Player {
	return &Player{ControlURL: controlURL, client: &http.Client{Timeout: 10 * time.Second}}
}

func (p *Player) soap(action, body string) error {
	envelope := `<?xml version="1.0" encoding="utf-8"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body>` +
		body + `</s:Body></s:Envelope>`
	req, err := http.NewRequest(http.MethodPost, p.ControlURL, bytes.NewBufferString(envelope))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPACTION", fmt.Sprintf(`"%s#%s"`, avTransportType, action))
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s failed: %s: %s", action, resp.Status, strings.TrimSpace(string(b)))
	}
	return nil
}

// Play sets the stream URL on the renderer and starts playback. Metadata is left
// empty on purpose — the SoundTouch 20 rejects non-empty DIDL.
func (p *Player) Play(streamURL string) error {
	setURI := fmt.Sprintf(
		`<u:SetAVTransportURI xmlns:u="%s"><InstanceID>0</InstanceID>`+
			`<CurrentURI>%s</CurrentURI><CurrentURIMetaData></CurrentURIMetaData></u:SetAVTransportURI>`,
		avTransportType, xmlEscape(streamURL))
	if err := p.soap("SetAVTransportURI", setURI); err != nil {
		return err
	}
	play := fmt.Sprintf(
		`<u:Play xmlns:u="%s"><InstanceID>0</InstanceID><Speed>1</Speed></u:Play>`, avTransportType)
	return p.soap("Play", play)
}

func xmlEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

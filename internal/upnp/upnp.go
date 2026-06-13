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

// FindControlURL fetches the device description and returns the AVTransport
// control URL. SoundTouch exposes the description on port 8091/8092.
func FindControlURL(host string) (string, error) {
	hc := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for _, port := range []int{8091, 8092} {
		descURL := fmt.Sprintf("http://%s:%d/DeviceDescription.xml", host, port)
		resp, err := hc.Get(descURL)
		if err != nil {
			lastErr = err
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		var root descRoot
		if err := xml.Unmarshal(body, &root); err != nil {
			lastErr = err
			continue
		}
		var svcs []service
		collectServices(root.Device, &svcs)
		for _, s := range svcs {
			if strings.Contains(s.ServiceType, "AVTransport") && s.ControlURL != "" {
				base := root.URLBase
				if base == "" {
					base = fmt.Sprintf("http://%s:%d/", host, port)
				}
				return resolveRef(base, s.ControlURL), nil
			}
		}
		lastErr = fmt.Errorf("no AVTransport service in description at %s", descURL)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("device description not found on %s", host)
	}
	return "", lastErr
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

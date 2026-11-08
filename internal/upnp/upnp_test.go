package upnp

import (
	"encoding/xml"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

// The ID in testdata/soundtouch20-description.xml and boseSSDP below. Both are
// real SoundTouch 20 (firmware 27.x) responses with the device ID replaced.
const testID = "A0B1C2D3E4F5"

// SSDP responses captured on a LAN with this speaker, a Samsung TV, and Sonos
// players, IDs replaced. All of them answer the MediaRenderer M-SEARCH, and the
// others reply before the Bose does. Note the formats: Bose sends "USN:uuid:"
// with no space and a mixed-case "Location:"; Sonos sends extra headers that
// start with or contain "LOCATION" but are not the description URL.
const (
	boseSSDP = "HTTP/1.1 200 OK\r\n" +
		"ST:urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN:uuid:BO5EBO5E-F00D-F00D-FEED-" + testID + "::urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"Location: http://192.168.1.29:8091/XD/BO5EBO5E-F00D-F00D-FEED-" + testID + ".xml\r\n" +
		"Ext:\r\n" +
		"Server: Linux/3.14.43+ UPnP/1.0 GUPnP/1.0.5\r\n" +
		"Cache-Control: max-age=1800\r\n" +
		"Content-Length: 0\r\n\r\n"

	sonosSSDP = "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age = 1800\r\n" +
		"EXT:\r\n" +
		"LOCATION.SMARTSPEAKER.AUDIO: lc_00000000000000000000000000000000\r\n" +
		"SECURELOCATION.UPNP.ORG: https://192.168.1.25:1443/xml/device_description.xml\r\n" +
		"LOCATION: http://192.168.1.25:1400/xml/device_description.xml\r\n" +
		"SERVER: Linux UPnP/1.0 Sonos/98.0-82170 (ZPS14)\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:RINCON_000000000000001400_MR::urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"

	samsungSSDP = "HTTP/1.1 200 OK\r\n" +
		"CACHE-CONTROL: max-age=1800\r\n" +
		"EXT: \r\n" +
		"LOCATION: http://192.168.1.28:9197/dmr\r\n" +
		"SERVER: Samsung-Linux/4.1, UPnP/1.0, Samsung_UPnP_SDK/1.0\r\n" +
		"ST: urn:schemas-upnp-org:device:MediaRenderer:1\r\n" +
		"USN: uuid:00000000-0000-0000-0000-000000000000::urn:schemas-upnp-org:device:MediaRenderer:1\r\n\r\n"
)

var (
	boseIP    = net.ParseIP("192.168.1.29")
	sonosIP   = net.ParseIP("192.168.1.25")
	samsungIP = net.ParseIP("192.168.1.28")
	boseLoc   = "http://192.168.1.29:8091/XD/BO5EBO5E-F00D-F00D-FEED-" + testID + ".xml"
)

func TestMatchSSDP(t *testing.T) {
	cases := []struct {
		name     string
		payload  string
		from     net.IP
		host, id string
		want     string // "" = rejected
	}{
		// On the speaker (host 127.0.0.1) every renderer's reply arrives from
		// some LAN address, so the device ID is the only thing telling them apart.
		{"on-device: bose", boseSSDP, boseIP, "127.0.0.1", testID, boseLoc},
		{"on-device: sonos rejected", sonosSSDP, sonosIP, "127.0.0.1", testID, ""},
		{"on-device: samsung rejected", samsungSSDP, samsungIP, "127.0.0.1", testID, ""},
		{"device id is case-insensitive", boseSSDP, boseIP, "127.0.0.1", strings.ToLower(testID), boseLoc},

		// Off-device (development), the source address must also match the host.
		{"remote: bose", boseSSDP, boseIP, "192.168.1.29", testID, boseLoc},
		{"remote: reply from another address", boseSSDP, sonosIP, "192.168.1.29", testID, ""},

		// Without a device ID any renderer is accepted, which is why main always
		// passes one. Pinned here so a change to that is deliberate.
		{"no id: first renderer wins", samsungSSDP, samsungIP, "localhost", "", "http://192.168.1.28:9197/dmr"},

		// The description URL, not Sonos's LOCATION.SMARTSPEAKER.AUDIO token or
		// its https SECURELOCATION, both of which come earlier in the reply.
		{"sonos: real LOCATION header", sonosSSDP, sonosIP, "192.168.1.25", "", "http://192.168.1.25:1400/xml/device_description.xml"},

		{"no location header", "HTTP/1.1 200 OK\r\nUSN:uuid:x-" + testID + "\r\n\r\n", boseIP, "127.0.0.1", testID, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := matchSSDP(c.payload, c.from, c.host, c.id)
			if ok != (c.want != "") || got != c.want {
				t.Errorf("got %q, %v; want %q", got, ok, c.want)
			}
		})
	}
}

func TestCandidates(t *testing.T) {
	ssdpOK := func(string, string) (string, error) { return "http://ssdp/desc.xml", nil }
	ssdpFail := func(string, string) (string, error) { return "", errors.New("i/o timeout") }

	direct := "http://127.0.0.1:8091/XD/BO5EBO5E-F00D-F00D-FEED-" + testID + ".xml"
	cases := []struct {
		name string
		id   string
		ssdp func(string, string) (string, error)
		want []string
	}{
		// The ID-derived URL is definitively this speaker, so it goes first.
		{"id + ssdp", strings.ToLower(testID), ssdpOK, []string{direct, "http://ssdp/desc.xml"}},
		{"id, ssdp timed out", testID, ssdpFail, []string{direct}},
		{"no id", "", ssdpOK, []string{"http://ssdp/desc.xml"}},
		{"nothing", "", ssdpFail, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := candidates("127.0.0.1", c.id, c.ssdp)
			if strings.Join(got, " ") != strings.Join(c.want, " ") {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// descServer serves body at /desc.xml and returns that URL.
func descServer(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/desc.xml"
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestControlURLFromDesc(t *testing.T) {
	// The real description: AVTransport among ConnectionManager,
	// RenderingControl and QPlay, a relative controlURL, no URLBase, and a
	// default XML namespace.
	loc := descServer(t, 200, readFixture(t, "soundtouch20-description.xml"))
	got, err := controlURLFromDesc(loc)
	want := strings.TrimSuffix(loc, "/desc.xml") + "/AVTransport/Control"
	if err != nil || got != want {
		t.Errorf("soundtouch 20: got %q, %v; want %q", got, err, want)
	}

	cases := []struct {
		name, body string
		want       string // "" = error expected; "{base}" = the server's base URL
	}{
		{"URLBase overrides the description URL",
			`<root><URLBase>http://10.0.0.5:49152/</URLBase><device><serviceList><service>
			<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
			<controlURL>AVT/ctrl</controlURL></service></serviceList></device></root>`,
			"http://10.0.0.5:49152/AVT/ctrl"},
		{"absolute controlURL kept",
			`<root><device><serviceList><service>
			<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
			<controlURL>http://10.0.0.6:1400/MediaRenderer/AVTransport/Control</controlURL>
			</service></serviceList></device></root>`,
			"http://10.0.0.6:1400/MediaRenderer/AVTransport/Control"},
		{"AVTransport on an embedded device (Sonos layout)",
			`<root><device><serviceList><service>
			<serviceType>urn:schemas-upnp-org:service:AlarmClock:1</serviceType>
			<controlURL>/AlarmClock/Control</controlURL></service></serviceList>
			<deviceList><device><serviceList><service>
			<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
			<controlURL>/MediaRenderer/AVTransport/Control</controlURL>
			</service></serviceList></device></deviceList></device></root>`,
			"{base}/MediaRenderer/AVTransport/Control"},
		{"AVTransport without a controlURL is skipped",
			`<root><device><serviceList><service>
			<serviceType>urn:schemas-upnp-org:service:AVTransport:1</serviceType>
			</service></serviceList></device></root>`,
			""},
		{"no AVTransport service",
			`<root><device><serviceList><service>
			<serviceType>urn:schemas-upnp-org:service:RenderingControl:1</serviceType>
			<controlURL>/RenderingControl/Control</controlURL>
			</service></serviceList></device></root>`,
			""},
		{"not XML", `{"error":"nope"}`, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			loc := descServer(t, 200, c.body)
			want := strings.Replace(c.want, "{base}", strings.TrimSuffix(loc, "/desc.xml"), 1)
			got, err := controlURLFromDesc(loc)
			if want == "" {
				if err == nil {
					t.Errorf("got %q, want an error", got)
				}
				return
			}
			if err != nil || got != want {
				t.Errorf("got %q, %v; want %q", got, err, want)
			}
		})
	}

	// A 404 page is not a description.
	if got, err := controlURLFromDesc(descServer(t, 404, "<html>not found</html>")); err == nil {
		t.Errorf("404: got %q, want an error", got)
	}
}

func TestFirstControlURL(t *testing.T) {
	good := descServer(t, 200, readFixture(t, "soundtouch20-description.xml"))
	wantGood := strings.TrimSuffix(good, "/desc.xml") + "/AVTransport/Control"
	bad := descServer(t, 200, "<root><device/></root>")
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/desc.xml"
	dead.Close() // connection refused

	// Falls through a dead and a useless description to a working one.
	if got, err := firstControlURL([]string{deadURL, bad, good}, "h"); err != nil || got != wantGood {
		t.Errorf("fallthrough: got %q, %v; want %q", got, err, wantGood)
	}

	// All fail: the last error is reported, not a generic one.
	_, err := firstControlURL([]string{deadURL, bad}, "h")
	if err == nil || !strings.Contains(err.Error(), "no AVTransport service") {
		t.Errorf("all fail: got %v, want the last description's error", err)
	}

	// No candidates at all.
	_, err = firstControlURL(nil, "192.168.1.29")
	if err == nil || !strings.Contains(err.Error(), "not found for 192.168.1.29") {
		t.Errorf("no candidates: got %v", err)
	}
}

// soapCall is one request as the renderer received it.
type soapCall struct {
	action, contentType string
	body                []byte
}

// renderer is a fake AVTransport endpoint. fail maps a SOAP action to the
// status it should answer with.
func renderer(t *testing.T, fail map[string]int) (*Player, func() []soapCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []soapCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		action := r.Header.Get("SOAPACTION")
		mu.Lock()
		calls = append(calls, soapCall{action, r.Header.Get("Content-Type"), b})
		mu.Unlock()
		if r.Method != http.MethodPost {
			t.Errorf("%s: method %s, want POST", action, r.Method)
		}
		for name, status := range fail {
			if strings.HasSuffix(action, "#"+name+`"`) {
				w.WriteHeader(status)
				io.WriteString(w, `<s:Envelope><s:Body><s:Fault><detail><UPnPError><errorCode>714</errorCode>`+
					`<errorDescription>Illegal MIME-type</errorDescription></UPnPError></detail></s:Fault></s:Body></s:Envelope>`)
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL + "/AVTransport/Control"), func() []soapCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]soapCall(nil), calls...)
	}
}

// envelope decodes the parts of a SOAP body the renderer reads. encoding/xml
// matches on local names, so the s:/u: prefixes don't matter here.
type envelope struct {
	Body struct {
		SetURI *struct {
			InstanceID string  `xml:"InstanceID"`
			URI        string  `xml:"CurrentURI"`
			Meta       *string `xml:"CurrentURIMetaData"`
		} `xml:"SetAVTransportURI"`
		Play *struct {
			InstanceID string `xml:"InstanceID"`
			Speed      string `xml:"Speed"`
		} `xml:"Play"`
	} `xml:"Body"`
}

func TestPlay(t *testing.T) {
	p, calls := renderer(t, nil)
	// A query string with & would break the envelope if it weren't escaped.
	stream := "http://192.168.1.29:8099/stream/3?a=1&b=<2>"
	if err := p.Play(stream); err != nil {
		t.Fatal(err)
	}

	got := calls()
	if len(got) != 2 {
		t.Fatalf("got %d SOAP calls, want SetAVTransportURI then Play", len(got))
	}
	for i, want := range []string{"SetAVTransportURI", "Play"} {
		if got[i].action != `"urn:schemas-upnp-org:service:AVTransport:1#`+want+`"` {
			t.Errorf("call %d: SOAPACTION %s, want %s", i, got[i].action, want)
		}
		if !strings.HasPrefix(got[i].contentType, "text/xml") {
			t.Errorf("call %d: Content-Type %q", i, got[i].contentType)
		}
	}

	var set envelope
	if err := xml.Unmarshal(got[0].body, &set); err != nil {
		t.Fatalf("SetAVTransportURI body is not well-formed XML: %v\n%s", err, got[0].body)
	}
	u := set.Body.SetURI
	switch {
	case u == nil:
		t.Fatalf("no SetAVTransportURI element:\n%s", got[0].body)
	case u.URI != stream:
		t.Errorf("CurrentURI %q, want %q", u.URI, stream)
	case u.InstanceID != "0":
		t.Errorf("InstanceID %q, want 0", u.InstanceID)
	case u.Meta == nil || *u.Meta != "":
		// The SoundTouch 20 rejects non-empty DIDL, but the element must exist.
		t.Errorf("CurrentURIMetaData must be present and empty, got %v", u.Meta)
	}

	var play envelope
	if err := xml.Unmarshal(got[1].body, &play); err != nil {
		t.Fatalf("Play body is not well-formed XML: %v", err)
	}
	if pl := play.Body.Play; pl == nil || pl.InstanceID != "0" || pl.Speed != "1" {
		t.Errorf("Play: got %+v, want InstanceID 0, Speed 1", pl)
	}
}

func TestPlayErrors(t *testing.T) {
	// SetAVTransportURI rejected: report the renderer's fault and don't send Play.
	p, calls := renderer(t, map[string]int{"SetAVTransportURI": 500})
	err := p.Play("http://192.168.1.29:8099/stream/1")
	if err == nil || !strings.Contains(err.Error(), "SetAVTransportURI failed: 500") ||
		!strings.Contains(err.Error(), "Illegal MIME-type") {
		t.Errorf("got %v, want the status and the renderer's fault text", err)
	}
	if n := len(calls()); n != 1 {
		t.Errorf("%d calls, want Play not sent after a failed SetAVTransportURI", n)
	}

	// Play rejected.
	p, _ = renderer(t, map[string]int{"Play": 500})
	if err := p.Play("http://192.168.1.29:8099/stream/1"); err == nil || !strings.Contains(err.Error(), "Play failed: 500") {
		t.Errorf("got %v, want a Play failure", err)
	}

	// Renderer unreachable.
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	if err := New(dead.URL).Play("http://x/stream/1"); err == nil {
		t.Error("unreachable renderer: want an error")
	}
}

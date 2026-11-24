package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// UPnP IGD: an SSDP search for the gateway device, its description, then
// SOAP calls on its WANIPConnection or WANPPPConnection service. Every URL
// followed must name the gateway that answered, by IP address: a reply
// cannot point this machine at anything else.

const (
	ssdpDefault = "239.255.255.250:1900"
	igdDevice   = "urn:schemas-upnp-org:device:InternetGatewayDevice:1"
	maxDescBody = 64 << 10
	description = "a2p2"
)

// upnpService is the control point found for the gateway.
type upnpService struct {
	control string
	service string
}

func (c *Client) ssdp() string {
	if c.SSDP == "" {
		return ssdpDefault
	}
	return c.SSDP
}

// discover sends one M-SEARCH and follows the first answer from the
// gateway to its WAN connection service.
func (c *Client) discover(ctx context.Context) (upnpService, error) {
	to, err := net.ResolveUDPAddr("udp", c.ssdp())
	if err != nil {
		return upnpService{}, err
	}
	conn, err := net.ListenUDP("udp", nil)
	if err != nil {
		return upnpService{}, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stop()
	q := "M-SEARCH * HTTP/1.1\r\nHOST: " + ssdpDefault + "\r\nST: " + igdDevice +
		"\r\nMAN: \"ssdp:discover\"\r\nMX: 2\r\n\r\n"
	wait, tries := c.tries()
	buf := make([]byte, 4096)
	for range tries {
		if _, err := conn.WriteToUDP([]byte(q), to); err != nil {
			return upnpService{}, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(wait))
		for {
			n, from, err := conn.ReadFromUDPAddrPort(buf)
			if ctx.Err() != nil {
				return upnpService{}, ctx.Err()
			}
			if err != nil {
				break
			}
			if from.Addr().Unmap() != c.Gateway {
				continue // only the gateway's answer counts
			}
			resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(buf[:n])), nil)
			if err != nil {
				continue
			}
			loc := resp.Header.Get("Location")
			resp.Body.Close()
			if !c.gatewayURL(loc) {
				continue
			}
			return c.describe(ctx, loc)
		}
		wait *= 2
	}
	return upnpService{}, errNoAnswer
}

// gatewayURL accepts only an http URL whose host is the gateway's IP.
func (c *Client) gatewayURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "http" {
		return false
	}
	ip, err := netip.ParseAddr(u.Hostname())
	return err == nil && ip.Unmap() == c.Gateway
}

type upnpDesc struct {
	URLBase string     `xml:"URLBase"`
	Device  upnpDevice `xml:"device"`
}

type upnpDevice struct {
	Services []struct {
		Type    string `xml:"serviceType"`
		Control string `xml:"controlURL"`
	} `xml:"serviceList>service"`
	Devices []upnpDevice `xml:"deviceList>device"`
}

func (d upnpDevice) find() (typ, control string) {
	for _, s := range d.Services {
		if strings.Contains(s.Type, ":WANIPConnection:") || strings.Contains(s.Type, ":WANPPPConnection:") {
			return strings.TrimSpace(s.Type), strings.TrimSpace(s.Control)
		}
	}
	for _, sub := range d.Devices {
		if t, c := sub.find(); t != "" {
			return t, c
		}
	}
	return "", ""
}

func (c *Client) describe(ctx context.Context, loc string) (upnpService, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
	if err != nil {
		return upnpService{}, err
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return upnpService{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return upnpService{}, fmt.Errorf("description: %s", resp.Status)
	}
	var d upnpDesc
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxDescBody)).Decode(&d); err != nil {
		return upnpService{}, fmt.Errorf("description: %w", err)
	}
	typ, control := d.Device.find()
	if typ == "" {
		return upnpService{}, errors.New("the gateway offers no WAN connection service")
	}
	base := loc
	if d.URLBase != "" {
		base = d.URLBase
	}
	bu, err := url.Parse(base)
	if err != nil {
		return upnpService{}, err
	}
	cu, err := bu.Parse(control)
	if err != nil || !c.gatewayURL(cu.String()) {
		return upnpService{}, errors.New("the control URL is not on the gateway")
	}
	return upnpService{control: cu.String(), service: typ}, nil
}

// soap calls action with args (in order) and returns the response's
// fields by name.
func (c *Client) soap(ctx context.Context, s upnpService, action string, args [][2]string) (map[string]string, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" ` +
		`s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/"><s:Body><u:` + action + ` xmlns:u="`)
	_ = xml.EscapeText(&b, []byte(s.service))
	b.WriteString(`">`)
	for _, a := range args {
		b.WriteString("<" + a[0] + ">")
		_ = xml.EscapeText(&b, []byte(a[1]))
		b.WriteString("</" + a[0] + ">")
	}
	b.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.control, strings.NewReader(b.String()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+s.service+"#"+action+`"`)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	fields := map[string]string{}
	dec := xml.NewDecoder(io.LimitReader(resp.Body, maxDescBody))
	var name string
	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			name = t.Name.Local
		case xml.CharData:
			if name != "" {
				fields[name] += strings.TrimSpace(string(t))
			}
		case xml.EndElement:
			name = ""
		}
	}
	if resp.StatusCode != http.StatusOK {
		if code := fields["errorCode"]; code != "" {
			return fields, &upnpError{code: code, text: fields["errorDescription"]}
		}
		return fields, fmt.Errorf("%s: %s", action, resp.Status)
	}
	return fields, nil
}

type upnpError struct{ code, text string }

func (e *upnpError) Error() string {
	if e.text != "" {
		return "error " + e.code + " " + e.text
	}
	return "error " + e.code
}

func (c *Client) mapUPnP(ctx context.Context, internal uint16, lifetime time.Duration, prev *Mapping) (Mapping, error) {
	if !c.Local.IsValid() {
		return Mapping{}, errors.New("no local address on the gateway's network")
	}
	var s upnpService
	if prev != nil && prev.Protocol == UPnP && prev.control != "" {
		s = upnpService{control: prev.control, service: prev.service}
	} else {
		var err error
		if s, err = c.discover(ctx); err != nil {
			return Mapping{}, err
		}
	}
	ext := internal
	if prev != nil && prev.External.IsValid() {
		ext = prev.External.Port()
	}
	lease := lifetime
	add := func(port uint16, lease time.Duration) error {
		_, err := c.soap(ctx, s, "AddPortMapping", [][2]string{{"NewRemoteHost", ""},
			{"NewExternalPort", strconv.Itoa(int(port))}, {"NewProtocol", "TCP"},
			{"NewInternalPort", strconv.Itoa(int(internal))}, {"NewInternalClient", c.Local.String()},
			{"NewEnabled", "1"}, {"NewPortMappingDescription", description},
			{"NewLeaseDuration", strconv.Itoa(int(lease / time.Second))}})
		return err
	}
	err := add(ext, lease)
	var ue *upnpError
	if errors.As(err, &ue) && ue.code == "725" { // only permanent leases: removed by Unmap only
		lease = 0
		err = add(ext, lease)
	}
	for i := 1; errors.As(err, &ue) && ue.code == "718" && i <= 3; i++ { // the port is taken: the next ones
		ext = internal + uint16(i)
		err = add(ext, lease)
	}
	if err != nil {
		return Mapping{}, err
	}
	f, err := c.soap(ctx, s, "GetExternalIPAddress", nil)
	if err != nil {
		return Mapping{}, err
	}
	ip, err := netip.ParseAddr(f["NewExternalIPAddress"])
	if err != nil {
		return Mapping{}, errors.New("the gateway gave no external address")
	}
	m := Mapping{Protocol: UPnP, Internal: internal, External: netip.AddrPortFrom(ip.Unmap(), ext), Lifetime: lease,
		control: s.control, service: s.service}
	if lease > 0 {
		m.Expires = c.now().Add(lease)
	}
	return m, nil
}

func (c *Client) upnpDelete(ctx context.Context, m Mapping) error {
	_, err := c.soap(ctx, upnpService{control: m.control, service: m.service}, "DeletePortMapping",
		[][2]string{{"NewRemoteHost", ""}, {"NewExternalPort", strconv.Itoa(int(m.External.Port()))},
			{"NewProtocol", "TCP"}})
	return err
}

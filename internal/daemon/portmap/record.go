package portmap

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// maxRecorded is the most mappings one record holds: a run holds one, and
// at most a few more while a renewal or a removal is pending.
const maxRecorded = 16

// maxLifetime is the longest lifetime any of the protocols can grant.
const maxLifetime = (1<<32 - 1) * time.Second

// recorded is a Mapping as the record file holds it. The external port is
// its own field: a mapping noted before the router answered has a port
// and no address yet, and a deletion needs that port.
type recorded struct {
	Protocol     string     `json:"protocol"`
	Gateway      string     `json:"gateway"`
	Local        string     `json:"local,omitempty"`
	Internal     uint16     `json:"internal_port"`
	ExternalIP   string     `json:"external_ip,omitempty"`
	ExternalPort uint16     `json:"external_port"`
	LifetimeS    int64      `json:"lifetime_s"`
	Expires      *time.Time `json:"expires,omitempty"`
	Nonce        string     `json:"nonce,omitempty"`
	Control      string     `json:"control,omitempty"`
	Service      string     `json:"service,omitempty"`
}

// MarshalJSON writes m as the record holds it.
func (m Mapping) MarshalJSON() ([]byte, error) {
	r := recorded{Protocol: m.Protocol, Gateway: m.Gateway.String(), Internal: m.Internal,
		ExternalPort: m.External.Port(), LifetimeS: int64(m.Lifetime / time.Second), Control: m.Control, Service: m.Service}
	if m.Local.IsValid() {
		r.Local = m.Local.String()
	}
	if m.External.Addr().IsValid() {
		r.ExternalIP = m.External.Addr().String()
	}
	if !m.Expires.IsZero() {
		e := m.Expires.UTC()
		r.Expires = &e
	}
	if m.Protocol == PCP {
		r.Nonce = hex.EncodeToString(m.Nonce[:])
	}
	return json.Marshal(r)
}

// UnmarshalJSON reads m strictly: an unknown field or a value out of
// range is an error (Valid).
func (m *Mapping) UnmarshalJSON(b []byte) error {
	var r recorded
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return err
	}
	var out Mapping
	var err error
	out.Protocol, out.Internal, out.Control, out.Service = r.Protocol, r.Internal, r.Control, r.Service
	if out.Gateway, err = netip.ParseAddr(r.Gateway); err != nil {
		return fmt.Errorf("gateway: %w", err)
	}
	if r.Local != "" {
		if out.Local, err = netip.ParseAddr(r.Local); err != nil {
			return fmt.Errorf("local: %w", err)
		}
	}
	var ip netip.Addr
	if r.ExternalIP != "" {
		if ip, err = netip.ParseAddr(r.ExternalIP); err != nil {
			return fmt.Errorf("external_ip: %w", err)
		}
	}
	out.External = netip.AddrPortFrom(ip, r.ExternalPort)
	if r.LifetimeS < 0 || time.Duration(r.LifetimeS)*time.Second > maxLifetime {
		return errors.New("lifetime_s out of range")
	}
	out.Lifetime = time.Duration(r.LifetimeS) * time.Second
	if r.Expires != nil {
		out.Expires = *r.Expires
	}
	if r.Nonce != "" {
		n, err := hex.DecodeString(r.Nonce)
		if err != nil || len(n) != len(out.Nonce) {
			return errors.New("nonce: not 12 bytes in hex")
		}
		copy(out.Nonce[:], n)
	}
	if err := out.Valid(); err != nil {
		return err
	}
	*m = out
	return nil
}

// Valid says whether m holds what removing it needs, and nothing that does
// not belong to its protocol.
func (m Mapping) Valid() error {
	ok := func(a netip.Addr) bool {
		return a.IsValid() && a.Zone() == "" && !a.IsUnspecified() && !a.IsMulticast()
	}
	switch {
	case !ok(m.Gateway):
		return errors.New("no usable gateway address")
	case m.Internal == 0:
		return errors.New("no internal port")
	case m.Local.IsValid() && !ok(m.Local):
		return errors.New("local is not a usable address")
	case m.External.Addr().IsValid() && !ok(m.External.Addr()):
		return errors.New("external_ip is not a usable address")
	case m.Lifetime < 0 || m.Lifetime > maxLifetime:
		return errors.New("lifetime out of range")
	}
	var zero [12]byte
	switch m.Protocol {
	case PCP:
		if m.Nonce == zero || m.Control != "" || m.Service != "" {
			return errors.New("a PCP mapping needs its nonce and nothing of UPnP")
		}
	case NATPMP:
		if m.Nonce != zero || m.Control != "" || m.Service != "" {
			return errors.New("a NAT-PMP mapping has no nonce and nothing of UPnP")
		}
	case UPnP:
		u, err := url.Parse(m.Control)
		if err != nil || u.Scheme != "http" || u.Hostname() != m.Gateway.String() {
			return errors.New("a UPnP control URL must be http on the gateway's address")
		}
		if !strings.Contains(m.Service, ":WANIPConnection:") && !strings.Contains(m.Service, ":WANPPPConnection:") {
			return errors.New("not a UPnP WAN connection service")
		}
		if m.External.Port() == 0 || !ok(m.Local) || m.Nonce != zero {
			return errors.New("a UPnP mapping needs its external port and this machine's address")
		}
	default:
		return fmt.Errorf("unknown protocol %q", m.Protocol)
	}
	return nil
}

// File is a Record kept in one file (home/portmap.json). It is written to
// a new private temporary file, synced and renamed over the old one, and
// the directory synced, before each request is sent. Load reads it
// strictly: anything it cannot use whole is an error, and the file stays.
type File struct {
	Path string
	// Gateway, if set, is this network's router: a record of mappings on
	// any other is not used.
	Gateway netip.Addr
}

// ErrUnusable wraps every reason Load refuses a record.
var ErrUnusable = errors.New("the record of earlier port mappings cannot be used")

// Load reads the record; a missing file is an empty one.
func (f *File) Load() ([]Mapping, error) {
	b, err := os.ReadFile(f.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w (%s): %v", ErrUnusable, f.Path, err)
	}
	var out []Mapping
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("%w (%s): %v", ErrUnusable, f.Path, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("%w (%s): data after the list", ErrUnusable, f.Path)
	}
	if len(out) > maxRecorded {
		return nil, fmt.Errorf("%w (%s): %d mappings, more than %d", ErrUnusable, f.Path, len(out), maxRecorded)
	}
	for _, m := range out {
		if f.Gateway.IsValid() && m.Gateway != f.Gateway {
			return nil, fmt.Errorf("%w (%s): %s is not on this network's router %s", ErrUnusable, f.Path, m, f.Gateway)
		}
	}
	return out, nil
}

// Save replaces the record with held; an empty one removes the file.
func (f *File) Save(held []Mapping) error {
	dir := filepath.Dir(f.Path)
	if len(held) == 0 {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncDir(dir)
	}
	b, err := json.Marshal(held)
	if err != nil {
		return err
	}
	// CreateTemp opens a new file exclusively with mode 0600.
	tmp, err := os.CreateTemp(dir, filepath.Base(f.Path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(b)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(name, f.Path)
	}
	if err != nil {
		_ = os.Remove(name)
		return err
	}
	return syncDir(dir)
}

// syncDir makes a rename or removal in dir durable. Windows cannot sync a
// directory; there the rename is as durable as the system makes it.
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

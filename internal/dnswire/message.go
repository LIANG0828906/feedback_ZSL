// Package dnswire implements the subset of the DNS wire format used by the
// simulator. It deliberately has no dependencies outside the standard library.
package dnswire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
)

const (
	TypeA     Type = 1
	TypeNS    Type = 2
	TypeCNAME Type = 5
	TypeSOA   Type = 6
	TypeTXT   Type = 16
	TypeAAAA  Type = 28
	TypeOPT   Type = 41
	TypeANY   Type = 255

	ClassIN uint16 = 1

	RCodeNoError  uint8 = 0
	RCodeFormErr  uint8 = 1
	RCodeServFail uint8 = 2
	RCodeNXDomain uint8 = 3
	RCodeNotImp   uint8 = 4
	RCodeRefused  uint8 = 5
)

type Type uint16

func (t Type) String() string {
	switch t {
	case TypeA:
		return "A"
	case TypeNS:
		return "NS"
	case TypeCNAME:
		return "CNAME"
	case TypeSOA:
		return "SOA"
	case TypeTXT:
		return "TXT"
	case TypeAAAA:
		return "AAAA"
	case TypeOPT:
		return "OPT"
	case TypeANY:
		return "ANY"
	default:
		return fmt.Sprintf("TYPE%d", uint16(t))
	}
}

func ParseType(s string) (Type, error) {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "", "A":
		return TypeA, nil
	case "NS":
		return TypeNS, nil
	case "CNAME":
		return TypeCNAME, nil
	case "SOA":
		return TypeSOA, nil
	case "TXT":
		return TypeTXT, nil
	case "AAAA":
		return TypeAAAA, nil
	case "ANY":
		return TypeANY, nil
	default:
		return 0, fmt.Errorf("unsupported DNS type %q", s)
	}
}

type SOA struct {
	MName   string
	RName   string
	Serial  uint32
	Refresh uint32
	Retry   uint32
	Expire  uint32
	Minimum uint32
}

// RR uses one struct for all supported record types. Only the fields relevant
// to Type are populated.
type RR struct {
	Name   string
	Type   Type
	Class  uint16
	TTL    uint32
	A      string
	Target string
	Text   []string
	SOA    *SOA
	Raw    []byte
}

type Question struct {
	Name  string
	Type  Type
	Class uint16
}

type Option struct {
	Code uint16
	Data []byte
}

type Message struct {
	ID                 uint16
	Response           bool
	Opcode             uint8
	Authoritative      bool
	Truncated          bool
	RecursionDesired   bool
	RecursionAvailable bool
	RCode              uint8
	Questions          []Question
	Answers            []RR
	Authorities        []RR
	Additionals        []RR
	UDPPayloadSize     uint16
	Options            []Option
}

func (m Message) Question() (Question, bool) {
	if len(m.Questions) == 0 {
		return Question{}, false
	}
	return m.Questions[0], true
}

func (m Message) Option(code uint16) ([]byte, bool) {
	for _, opt := range m.Options {
		if opt.Code == code {
			return opt.Data, true
		}
	}
	return nil, false
}

func (m *Message) AddOption(code uint16, data []byte) {
	m.Options = append(m.Options, Option{Code: code, Data: append([]byte(nil), data...)})
}

func Parse(packet []byte) (Message, error) {
	var m Message
	if len(packet) < 12 {
		return m, errors.New("dns: packet shorter than header")
	}
	m.ID = binary.BigEndian.Uint16(packet[0:2])
	flags := binary.BigEndian.Uint16(packet[2:4])
	m.Response = flags&0x8000 != 0
	m.Opcode = uint8((flags >> 11) & 0x0f)
	m.Authoritative = flags&0x0400 != 0
	m.Truncated = flags&0x0200 != 0
	m.RecursionDesired = flags&0x0100 != 0
	m.RecursionAvailable = flags&0x0080 != 0
	m.RCode = uint8(flags & 0x0f)

	qd := int(binary.BigEndian.Uint16(packet[4:6]))
	an := int(binary.BigEndian.Uint16(packet[6:8]))
	ns := int(binary.BigEndian.Uint16(packet[8:10]))
	ar := int(binary.BigEndian.Uint16(packet[10:12]))
	if qd > 64 || an > 4096 || ns > 4096 || ar > 4096 {
		return m, errors.New("dns: unreasonable section count")
	}

	off := 12
	for i := 0; i < qd; i++ {
		name, next, err := unpackName(packet, off)
		if err != nil {
			return m, err
		}
		if next+4 > len(packet) {
			return m, errors.New("dns: truncated question")
		}
		m.Questions = append(m.Questions, Question{
			Name:  name,
			Type:  Type(binary.BigEndian.Uint16(packet[next : next+2])),
			Class: binary.BigEndian.Uint16(packet[next+2 : next+4]),
		})
		off = next + 4
	}

	var err error
	m.Answers, off, err = readRRs(packet, off, an, &m)
	if err != nil {
		return m, err
	}
	m.Authorities, off, err = readRRs(packet, off, ns, &m)
	if err != nil {
		return m, err
	}
	m.Additionals, off, err = readRRs(packet, off, ar, &m)
	if err != nil {
		return m, err
	}
	if off != len(packet) {
		return m, errors.New("dns: trailing bytes")
	}
	return m, nil
}

func readRRs(packet []byte, off, count int, msg *Message) ([]RR, int, error) {
	out := make([]RR, 0, count)
	for i := 0; i < count; i++ {
		name, next, err := unpackName(packet, off)
		if err != nil {
			return nil, off, err
		}
		if next+10 > len(packet) {
			return nil, off, errors.New("dns: truncated resource record")
		}
		rrType := Type(binary.BigEndian.Uint16(packet[next : next+2]))
		class := binary.BigEndian.Uint16(packet[next+2 : next+4])
		ttl := binary.BigEndian.Uint32(packet[next+4 : next+8])
		rdlen := int(binary.BigEndian.Uint16(packet[next+8 : next+10]))
		rdataOff := next + 10
		if rdataOff+rdlen > len(packet) {
			return nil, off, errors.New("dns: truncated rdata")
		}
		rdata := packet[rdataOff : rdataOff+rdlen]

		if rrType == TypeOPT {
			msg.UDPPayloadSize = class
			msg.Options = append(msg.Options, parseOptions(rdata)...)
			off = rdataOff + rdlen
			continue
		}

		rr := RR{Name: name, Type: rrType, Class: class, TTL: ttl}
		switch rrType {
		case TypeA:
			if rdlen != 4 {
				return nil, off, errors.New("dns: invalid A rdata")
			}
			rr.A = net.IP(append([]byte(nil), rdata...)).String()
		case TypeAAAA:
			if rdlen != 16 {
				return nil, off, errors.New("dns: invalid AAAA rdata")
			}
			rr.A = net.IP(append([]byte(nil), rdata...)).String()
		case TypeNS, TypeCNAME:
			target, _, err := unpackName(packet, rdataOff)
			if err != nil {
				return nil, off, err
			}
			rr.Target = target
		case TypeTXT:
			pos := 0
			for pos < len(rdata) {
				n := int(rdata[pos])
				pos++
				if pos+n > len(rdata) {
					return nil, off, errors.New("dns: invalid TXT rdata")
				}
				rr.Text = append(rr.Text, string(rdata[pos:pos+n]))
				pos += n
			}
		case TypeSOA:
			mname, p, err := unpackName(packet, rdataOff)
			if err != nil {
				return nil, off, err
			}
			rname, p, err := unpackName(packet, p)
			if err != nil {
				return nil, off, err
			}
			if p+20 > rdataOff+rdlen {
				return nil, off, errors.New("dns: invalid SOA rdata")
			}
			rr.SOA = &SOA{
				MName:   mname,
				RName:   rname,
				Serial:  binary.BigEndian.Uint32(packet[p : p+4]),
				Refresh: binary.BigEndian.Uint32(packet[p+4 : p+8]),
				Retry:   binary.BigEndian.Uint32(packet[p+8 : p+12]),
				Expire:  binary.BigEndian.Uint32(packet[p+12 : p+16]),
				Minimum: binary.BigEndian.Uint32(packet[p+16 : p+20]),
			}
		default:
			rr.Raw = append([]byte(nil), rdata...)
		}
		out = append(out, rr)
		off = rdataOff + rdlen
	}
	return out, off, nil
}

func parseOptions(rdata []byte) []Option {
	var out []Option
	for pos := 0; pos+4 <= len(rdata); {
		code := binary.BigEndian.Uint16(rdata[pos : pos+2])
		n := int(binary.BigEndian.Uint16(rdata[pos+2 : pos+4]))
		pos += 4
		if pos+n > len(rdata) {
			break
		}
		out = append(out, Option{Code: code, Data: append([]byte(nil), rdata[pos:pos+n]...)})
		pos += n
	}
	return out
}

func Pack(m Message) ([]byte, error) {
	buf := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(buf[0:2], m.ID)
	var flags uint16
	if m.Response {
		flags |= 0x8000
	}
	flags |= uint16(m.Opcode&0x0f) << 11
	if m.Authoritative {
		flags |= 0x0400
	}
	if m.Truncated {
		flags |= 0x0200
	}
	if m.RecursionDesired {
		flags |= 0x0100
	}
	if m.RecursionAvailable {
		flags |= 0x0080
	}
	flags |= uint16(m.RCode & 0x0f)
	binary.BigEndian.PutUint16(buf[2:4], flags)
	binary.BigEndian.PutUint16(buf[4:6], uint16(len(m.Questions)))
	binary.BigEndian.PutUint16(buf[6:8], uint16(len(m.Answers)))
	binary.BigEndian.PutUint16(buf[8:10], uint16(len(m.Authorities)))
	arCount := len(m.Additionals)
	if m.UDPPayloadSize > 0 || len(m.Options) > 0 {
		arCount++
	}
	binary.BigEndian.PutUint16(buf[10:12], uint16(arCount))

	var err error
	for _, q := range m.Questions {
		buf, err = appendName(buf, q.Name)
		if err != nil {
			return nil, err
		}
		buf = appendU16(buf, uint16(q.Type))
		buf = appendU16(buf, q.Class)
	}
	for _, section := range [][]RR{m.Answers, m.Authorities, m.Additionals} {
		for _, rr := range section {
			buf, err = appendRR(buf, rr)
			if err != nil {
				return nil, err
			}
		}
	}
	if m.UDPPayloadSize > 0 || len(m.Options) > 0 {
		buf, err = appendOPT(buf, m)
		if err != nil {
			return nil, err
		}
	}
	return buf, nil
}

func appendRR(buf []byte, rr RR) ([]byte, error) {
	var err error
	buf, err = appendName(buf, rr.Name)
	if err != nil {
		return nil, err
	}
	buf = appendU16(buf, uint16(rr.Type))
	class := rr.Class
	if class == 0 {
		class = ClassIN
	}
	buf = appendU16(buf, class)
	buf = appendU32(buf, rr.TTL)
	lenPos := len(buf)
	buf = append(buf, 0, 0)
	start := len(buf)

	switch rr.Type {
	case TypeA:
		ip := net.ParseIP(rr.A).To4()
		if ip == nil {
			return nil, fmt.Errorf("dns: invalid A value %q", rr.A)
		}
		buf = append(buf, ip...)
	case TypeAAAA:
		ip := net.ParseIP(rr.A)
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("dns: invalid AAAA value %q", rr.A)
		}
		buf = append(buf, ip.To16()...)
	case TypeNS, TypeCNAME:
		buf, err = appendName(buf, rr.Target)
		if err != nil {
			return nil, err
		}
	case TypeTXT:
		for _, text := range rr.Text {
			remaining := []byte(text)
			if len(remaining) == 0 {
				buf = append(buf, 0)
				continue
			}
			for len(remaining) > 0 {
				n := len(remaining)
				if n > 255 {
					n = 255
				}
				buf = append(buf, byte(n))
				buf = append(buf, remaining[:n]...)
				remaining = remaining[n:]
			}
		}
	case TypeSOA:
		if rr.SOA == nil {
			return nil, errors.New("dns: SOA record without data")
		}
		buf, err = appendName(buf, rr.SOA.MName)
		if err != nil {
			return nil, err
		}
		buf, err = appendName(buf, rr.SOA.RName)
		if err != nil {
			return nil, err
		}
		buf = appendU32(buf, rr.SOA.Serial)
		buf = appendU32(buf, rr.SOA.Refresh)
		buf = appendU32(buf, rr.SOA.Retry)
		buf = appendU32(buf, rr.SOA.Expire)
		buf = appendU32(buf, rr.SOA.Minimum)
	default:
		buf = append(buf, rr.Raw...)
	}

	n := len(buf) - start
	if n > 65535 {
		return nil, errors.New("dns: rdata too large")
	}
	binary.BigEndian.PutUint16(buf[lenPos:lenPos+2], uint16(n))
	return buf, nil
}

func appendOPT(buf []byte, m Message) ([]byte, error) {
	buf = append(buf, 0)
	buf = appendU16(buf, uint16(TypeOPT))
	size := m.UDPPayloadSize
	if size == 0 {
		size = 1232
	}
	buf = appendU16(buf, size)
	buf = appendU32(buf, 0)
	lenPos := len(buf)
	buf = append(buf, 0, 0)
	start := len(buf)
	for _, opt := range m.Options {
		if len(opt.Data) > 65535 {
			return nil, errors.New("dns: EDNS option too large")
		}
		buf = appendU16(buf, opt.Code)
		buf = appendU16(buf, uint16(len(opt.Data)))
		buf = append(buf, opt.Data...)
	}
	n := len(buf) - start
	if n > 65535 {
		return nil, errors.New("dns: EDNS options too large")
	}
	binary.BigEndian.PutUint16(buf[lenPos:lenPos+2], uint16(n))
	return buf, nil
}

func Truncate(m Message) Message {
	return Message{
		ID:                 m.ID,
		Response:           true,
		Opcode:             m.Opcode,
		Authoritative:      m.Authoritative,
		Truncated:          true,
		RecursionDesired:   m.RecursionDesired,
		RecursionAvailable: m.RecursionAvailable,
		RCode:              m.RCode,
		Questions:          append([]Question(nil), m.Questions...),
		UDPPayloadSize:     1232,
		Options:            append([]Option(nil), m.Options...),
	}
}

func CanonicalName(name string) (string, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return "", errors.New("empty DNS name")
	}
	if name == "." {
		return ".", nil
	}
	name = strings.TrimSuffix(name, ".")
	labels := strings.Split(name, ".")
	total := 1
	for _, label := range labels {
		if label == "" {
			return "", fmt.Errorf("invalid DNS name %q", name)
		}
		if len(label) > 63 {
			return "", fmt.Errorf("DNS label too long in %q", name)
		}
		total += len(label) + 1
	}
	if total > 255 {
		return "", fmt.Errorf("DNS name too long %q", name)
	}
	return name + ".", nil
}

func IsSubdomain(name, zone string) bool {
	name = strings.ToLower(name)
	zone = strings.ToLower(zone)
	if zone == "." {
		return true
	}
	return name == zone || strings.HasSuffix(name, "."+strings.TrimSuffix(zone, ".")+".")
}

func appendName(buf []byte, name string) ([]byte, error) {
	if name == "" || name == "." {
		return append(buf, 0), nil
	}
	canonical, err := CanonicalName(name)
	if err != nil {
		return nil, err
	}
	trimmed := strings.TrimSuffix(canonical, ".")
	for _, label := range strings.Split(trimmed, ".") {
		if len(label) == 0 || len(label) > 63 {
			return nil, fmt.Errorf("dns: invalid label in %q", name)
		}
		buf = append(buf, byte(len(label)))
		buf = append(buf, label...)
	}
	return append(buf, 0), nil
}

func unpackName(packet []byte, off int) (string, int, error) {
	if off < 0 || off >= len(packet) {
		return "", off, errors.New("dns: name offset out of range")
	}
	var labels []string
	next := -1
	visited := 0
	wireLen := 1
	for {
		if off >= len(packet) {
			return "", off, errors.New("dns: truncated name")
		}
		if visited > 128 {
			return "", off, errors.New("dns: compression pointer loop")
		}
		visited++
		n := int(packet[off])
		if n&0xc0 == 0xc0 {
			if off+1 >= len(packet) {
				return "", off, errors.New("dns: truncated compression pointer")
			}
			ptr := int(binary.BigEndian.Uint16(packet[off:off+2]) & 0x3fff)
			if ptr >= len(packet) {
				return "", off, errors.New("dns: compression pointer out of range")
			}
			if next < 0 {
				next = off + 2
			}
			off = ptr
			continue
		}
		if n&0xc0 != 0 {
			return "", off, errors.New("dns: unsupported label type")
		}
		off++
		if n == 0 {
			if next < 0 {
				next = off
			}
			break
		}
		if n > 63 || off+n > len(packet) {
			return "", off, errors.New("dns: invalid label length")
		}
		wireLen += n + 1
		if wireLen > 255 {
			return "", off, errors.New("dns: name too long")
		}
		labels = append(labels, strings.ToLower(string(packet[off:off+n])))
		off += n
	}
	if len(labels) == 0 {
		return ".", next, nil
	}
	return strings.Join(labels, ".") + ".", next, nil
}

func appendU16(buf []byte, v uint16) []byte {
	return append(buf, byte(v>>8), byte(v))
}

func appendU32(buf []byte, v uint32) []byte {
	return append(buf, byte(v>>24), byte(v>>16), byte(v>>8), byte(v))
}

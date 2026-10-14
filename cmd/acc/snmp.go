package main

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"net"
	"os"
	"sync"
	"time"
)

// Uptime over SNMP. ADM does not announce its uptime via mDNS, but its SNMP
// service (ADM > Services > SNMP) answers hrSystemUptime.0 and sysUpTime.0
// to anyone who knows the community. This is a GET client just big enough
// for that: the binary has to stay a single static file without
// dependencies, and a full SNMP library would be a hundred times the code.

// snmpPort is where the NAS listens for SNMP (a variable so tests can
// point the client at a fake agent).
var snmpPort = "161"

// Settings from the command line: --community and --no-snmp.
var (
	snmpCommunity string
	snmpDisabled  bool
)

// defaultCommunity is "public" unless ACC_SNMP_COMMUNITY says otherwise.
func defaultCommunity() string {
	if c := os.Getenv("ACC_SNMP_COMMUNITY"); c != "" {
		return c
	}
	return "public"
}

// Per-attempt timeouts. A NAS that answered SNMP before gets the patient
// treatment (a retry as well); an unknown one gets a single short attempt,
// so a NAS without SNMP cannot hold up the scan. A closed port answers at
// once with ICMP unreachable either way.
const (
	snmpTimeout        = time.Second
	snmpPatientTimeout = 1500 * time.Millisecond
)

// uptimeProbe runs SNMP uptime queries in the background. scan() starts
// them for remembered devices before discovery begins and collects the
// answers after it ends, so the queries hide behind the mDNS listen.
type uptimeProbe struct {
	mu      sync.Mutex
	queries map[string]*uptimeQuery
}

type uptimeQuery struct {
	done    chan struct{}
	seconds int64
	err     error
}

func newUptimeProbe() *uptimeProbe {
	return &uptimeProbe{queries: map[string]*uptimeQuery{}}
}

// ask starts a query to ip unless one is already running (or SNMP is
// off). patient is whether the NAS has answered SNMP in an earlier scan.
func (p *uptimeProbe) ask(ip string, patient bool) {
	if snmpDisabled || net.ParseIP(ip) == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, running := p.queries[ip]; running {
		return
	}
	q := &uptimeQuery{done: make(chan struct{})}
	p.queries[ip] = q
	timeout, retries := snmpTimeout, 0
	if patient {
		timeout, retries = snmpPatientTimeout, 1
	}
	go func() {
		defer close(q.done)
		q.seconds, q.err = snmpUptime(net.JoinHostPort(ip, snmpPort), snmpCommunity, timeout, retries)
	}()
}

// apply waits for the queries of the devices in list and records their
// uptime. Queries for devices that are not in the list (remembered ones
// that did not answer discovery) are left to time out on their own.
func (p *uptimeProbe) apply(list []Nas) {
	for i := range list {
		p.mu.Lock()
		q := p.queries[list[i].IP]
		p.mu.Unlock()
		if q == nil {
			continue
		}
		<-q.done
		if q.err != nil {
			vlog("snmp %s: %v", list[i].IP, q.err)
			continue
		}
		vlog("snmp %s: uptime %s", list[i].IP, formatDuration(time.Duration(q.seconds)*time.Second))
		seconds := q.seconds
		list[i].UptimeSeconds, list[i].UptimeSource, list[i].SNMP = &seconds, "snmp", true
	}
}

// The two uptime objects, in order of preference: hrSystemUptime is the
// time since the host booted, sysUpTime only since the SNMP agent started.
var (
	oidHrSystemUptime = []uint32{1, 3, 6, 1, 2, 1, 25, 1, 1, 0}
	oidSysUpTime      = []uint32{1, 3, 6, 1, 2, 1, 1, 3, 0}
)

// BER tags used by SNMP GET traffic.
const (
	tagInteger        = 0x02
	tagOctetString    = 0x04
	tagNull           = 0x05
	tagOID            = 0x06
	tagSequence       = 0x30
	tagTimeTicks      = 0x43
	tagNoSuchObject   = 0x80
	tagNoSuchInstance = 0x81
	tagEndOfMibView   = 0x82
	tagGetRequest     = 0xA0
	tagGetResponse    = 0xA2
)

const (
	snmpV1  = 0
	snmpV2c = 1
)

// maxDatagram caps what is read from the socket; an uptime reply is well
// under a hundred bytes.
const maxDatagram = 2048

var errMalformed = errors.New("malformed SNMP message")

// snmpUptime asks the agent at addr (host:port) for its uptime in seconds.
// It sends a v2c request, retries it `retries` times when nothing comes
// back, and then tries SNMPv1 once. timeout is per attempt. An agent that
// answers but has no uptime, and a closed port (ICMP unreachable), end the
// query at once.
func snmpUptime(addr, community string, timeout time.Duration, retries int) (int64, error) {
	conn, err := net.DialTimeout("udp", addr, timeout)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	versions := make([]int64, 0, retries+2)
	for i := 0; i <= retries; i++ {
		versions = append(versions, snmpV2c)
	}
	versions = append(versions, snmpV1)
	var lastErr error
	for _, version := range versions {
		ticks, err := uptimeTicks(conn, version, community, timeout)
		if err == nil {
			return int64(ticks / 100), nil
		}
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			return 0, err
		}
		lastErr = err
	}
	return 0, lastErr
}

// uptimeTicks fetches both uptime objects in one request and returns the
// preferred one that the agent has, in hundredths of a second.
func uptimeTicks(conn net.Conn, version int64, community string, timeout time.Duration) (uint32, error) {
	both := [][]uint32{oidHrSystemUptime, oidSysUpTime}
	resp, err := snmpGet(conn, version, community, both, timeout)
	if err != nil {
		return 0, err
	}
	if resp.errorStatus != 0 {
		// A v1 agent without the host resources MIB rejects the whole
		// request with noSuchName; ask again for sysUpTime alone.
		resp, err = snmpGet(conn, version, community, both[1:], timeout)
		if err != nil {
			return 0, err
		}
		if resp.errorStatus != 0 {
			return 0, fmt.Errorf("snmp error status %d", resp.errorStatus)
		}
	}
	for _, oid := range both {
		if ticks, ok := resp.ticks(oid); ok {
			return ticks, nil
		}
	}
	return 0, errors.New("agent reports no uptime")
}

// snmpGet sends one GetRequest and waits for the matching GetResponse.
// Datagrams that do not parse or answer another request (a late reply to
// an earlier attempt) are skipped until the deadline.
func snmpGet(conn net.Conn, version int64, community string, oids [][]uint32, timeout time.Duration) (*snmpResponse, error) {
	id := rand.Int32N(math.MaxInt32) + 1
	if _, err := conn.Write(encodeGetRequest(version, community, id, oids)); err != nil {
		return nil, err
	}
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	buf := make([]byte, maxDatagram)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			return nil, err
		}
		resp, err := decodeResponse(buf[:n])
		if err != nil {
			vlog("snmp %s: ignoring %d-byte datagram: %v", conn.RemoteAddr(), n, err)
			continue
		}
		if resp.requestID != id {
			vlog("snmp %s: ignoring reply to request %d", conn.RemoteAddr(), resp.requestID)
			continue
		}
		return resp, nil
	}
}

// snmpVarbind is one OID/value pair of a response. value holds the raw BER
// contents; tag tells how to read it (TimeTicks, or one of the v2c
// exceptions noSuchObject/noSuchInstance/endOfMibView).
type snmpVarbind struct {
	oid   []uint32
	tag   byte
	value []byte
}

type snmpResponse struct {
	version     int64
	community   string
	requestID   int32
	errorStatus int64
	errorIndex  int64
	varbinds    []snmpVarbind
}

// ticks returns the TimeTicks value bound to oid, if the response has one.
func (r *snmpResponse) ticks(oid []uint32) (uint32, bool) {
	for _, vb := range r.varbinds {
		if vb.tag != tagTimeTicks || !oidEqual(vb.oid, oid) {
			continue
		}
		if v, err := decodeTicks(vb.value); err == nil {
			return v, true
		}
	}
	return 0, false
}

// decodeTicks reads a TimeTicks value. Strictly that is a non-negative
// INTEGER, but agents that drop the leading zero byte of a value above
// 2^31 exist, so the bytes are read as unsigned.
func decodeTicks(val []byte) (uint32, error) {
	if len(val) == 0 || len(val) > 5 || (len(val) == 5 && val[0] != 0) {
		return 0, errMalformed
	}
	var v uint64
	for _, c := range val {
		v = v<<8 | uint64(c)
	}
	return uint32(v), nil
}

func oidEqual(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// encodeGetRequest builds a GetRequest message:
//
//	SEQUENCE { INTEGER version, OCTET STRING community,
//	  [0xA0] { INTEGER request-id, INTEGER 0, INTEGER 0,
//	    SEQUENCE OF SEQUENCE { OID, NULL } } }
func encodeGetRequest(version int64, community string, requestID int32, oids [][]uint32) []byte {
	var varbinds []byte
	for _, oid := range oids {
		varbinds = append(varbinds, berTLV(tagSequence, append(berOID(oid), berTLV(tagNull, nil)...))...)
	}
	var pdu []byte
	pdu = append(pdu, berInt(tagInteger, int64(requestID))...)
	pdu = append(pdu, berInt(tagInteger, 0)...)
	pdu = append(pdu, berInt(tagInteger, 0)...)
	pdu = append(pdu, berTLV(tagSequence, varbinds)...)
	var msg []byte
	msg = append(msg, berInt(tagInteger, version)...)
	msg = append(msg, berTLV(tagOctetString, []byte(community))...)
	msg = append(msg, berTLV(tagGetRequest, pdu)...)
	return berTLV(tagSequence, msg)
}

// berTLV wraps val in a tag and a definite length.
func berTLV(tag byte, val []byte) []byte {
	out := []byte{tag}
	switch n := len(val); {
	case n < 0x80:
		out = append(out, byte(n))
	case n < 0x100:
		out = append(out, 0x81, byte(n))
	case n < 0x10000:
		out = append(out, 0x82, byte(n>>8), byte(n))
	default:
		out = append(out, 0x83, byte(n>>16), byte(n>>8), byte(n))
	}
	return append(out, val...)
}

// berInt encodes v in the shortest two's complement form.
func berInt(tag byte, v int64) []byte {
	var b [8]byte
	for i := range b {
		b[i] = byte(v >> (8 * (7 - i)))
	}
	i := 0
	for i < 7 && ((b[i] == 0 && b[i+1]&0x80 == 0) || (b[i] == 0xFF && b[i+1]&0x80 != 0)) {
		i++
	}
	return berTLV(tag, b[i:])
}

// berOID encodes an object identifier: the first two arcs share a byte,
// every arc is base 128 with the high bit marking continuation.
func berOID(oid []uint32) []byte {
	var b []byte
	if len(oid) >= 2 {
		b = appendBase128(b, oid[0]*40+oid[1])
		for _, arc := range oid[2:] {
			b = appendBase128(b, arc)
		}
	}
	return berTLV(tagOID, b)
}

func appendBase128(b []byte, v uint32) []byte {
	var tmp [5]byte
	i := len(tmp) - 1
	tmp[i] = byte(v & 0x7F)
	for v >>= 7; v > 0; v >>= 7 {
		i--
		tmp[i] = byte(v&0x7F) | 0x80
	}
	return append(b, tmp[i:]...)
}

// readTLV splits the first element off b. Only definite lengths of at most
// three bytes are accepted: the indefinite form is not allowed in SNMP and
// anything longer cannot fit a datagram.
func readTLV(b []byte) (tag byte, val, rest []byte, err error) {
	if len(b) < 2 || b[0]&0x1F == 0x1F {
		return 0, nil, nil, errMalformed
	}
	tag = b[0]
	n, hdr := int(b[1]), 2
	if n&0x80 != 0 {
		k := n & 0x7F
		if k == 0 || k > 3 || len(b) < 2+k {
			return 0, nil, nil, errMalformed
		}
		n = 0
		for _, c := range b[2 : 2+k] {
			n = n<<8 | int(c)
		}
		hdr += k
	}
	if len(b)-hdr < n {
		return 0, nil, nil, errMalformed
	}
	return tag, b[hdr : hdr+n], b[hdr+n:], nil
}

// expectTLV is readTLV for an element whose tag is known in advance.
func expectTLV(b []byte, tag byte) (val, rest []byte, err error) {
	t, val, rest, err := readTLV(b)
	if err == nil && t != tag {
		err = errMalformed
	}
	return val, rest, err
}

// decodeInt reads a two's complement INTEGER of up to eight bytes.
func decodeInt(val []byte) (int64, error) {
	if len(val) == 0 || len(val) > 8 {
		return 0, errMalformed
	}
	v := int64(int8(val[0]))
	for _, c := range val[1:] {
		v = v<<8 | int64(c)
	}
	return v, nil
}

// decodeOID is the inverse of berOID. Arcs are limited to 32 bits and an
// identifier to 128 arcs, which covers every OID in SNMP use.
func decodeOID(val []byte) ([]uint32, error) {
	if len(val) == 0 {
		return nil, errMalformed
	}
	i := 0
	arc := func() (uint32, error) {
		var v uint64
		for n := 0; i < len(val); n++ {
			if n == 5 {
				return 0, errMalformed
			}
			c := val[i]
			i++
			v = v<<7 | uint64(c&0x7F)
			if c&0x80 == 0 {
				if v > math.MaxUint32 {
					return 0, errMalformed
				}
				return uint32(v), nil
			}
		}
		return 0, errMalformed
	}
	first, err := arc()
	if err != nil {
		return nil, err
	}
	var oid []uint32
	switch {
	case first < 40:
		oid = append(oid, 0, first)
	case first < 80:
		oid = append(oid, 1, first-40)
	default:
		oid = append(oid, 2, first-80)
	}
	for i < len(val) {
		if len(oid) == 128 {
			return nil, errMalformed
		}
		a, err := arc()
		if err != nil {
			return nil, err
		}
		oid = append(oid, a)
	}
	return oid, nil
}

// decodeResponse parses a GetResponse message.
func decodeResponse(pkt []byte) (*snmpResponse, error) {
	return decodeMessage(pkt, tagGetResponse)
}

// decodeMessage parses a message whose PDU has the given tag. The
// structure is fixed, so the parser is a straight sequence of reads with
// no recursion; every length is checked against what is left of the
// datagram.
func decodeMessage(pkt []byte, pduTag byte) (*snmpResponse, error) {
	msg, rest, err := expectTLV(pkt, tagSequence)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errMalformed
	}
	var r snmpResponse
	if r.version, msg, err = readInt(msg); err != nil {
		return nil, err
	}
	community, msg, err := expectTLV(msg, tagOctetString)
	if err != nil {
		return nil, err
	}
	r.community = string(community)
	pdu, rest, err := expectTLV(msg, pduTag)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errMalformed
	}
	id, pdu, err := readInt(pdu)
	if err != nil {
		return nil, err
	}
	if id < 0 || id > math.MaxInt32 {
		return nil, errMalformed
	}
	r.requestID = int32(id)
	if r.errorStatus, pdu, err = readInt(pdu); err != nil {
		return nil, err
	}
	if r.errorIndex, pdu, err = readInt(pdu); err != nil {
		return nil, err
	}
	list, rest, err := expectTLV(pdu, tagSequence)
	if err != nil {
		return nil, err
	}
	if len(rest) != 0 {
		return nil, errMalformed
	}
	for len(list) > 0 {
		var vb []byte
		if vb, list, err = expectTLV(list, tagSequence); err != nil {
			return nil, err
		}
		rawOID, vb, err := expectTLV(vb, tagOID)
		if err != nil {
			return nil, err
		}
		oid, err := decodeOID(rawOID)
		if err != nil {
			return nil, err
		}
		tag, value, rest, err := readTLV(vb)
		if err != nil {
			return nil, err
		}
		if len(rest) != 0 {
			return nil, errMalformed
		}
		r.varbinds = append(r.varbinds, snmpVarbind{oid: oid, tag: tag, value: value})
	}
	return &r, nil
}

func readInt(b []byte) (int64, []byte, error) {
	val, rest, err := expectTLV(b, tagInteger)
	if err != nil {
		return 0, nil, err
	}
	v, err := decodeInt(val)
	return v, rest, err
}

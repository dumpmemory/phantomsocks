package phantomtcp

import (
	"bytes"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

type RecordAddresses struct {
	TTL       int64
	Addresses []net.IP
}

type DNSRecords struct {
	Index int
	Hint  uint
	A     *RecordAddresses
	AAAA  *RecordAddresses
}

var DNSMinTTL uint32 = 0
var VirtualAddrPrefix byte = 255
var DNSCache sync.Map
var Nose []string = []string{"phantom.socks"}
var NoseLock sync.Mutex

func TCPlookup(request []byte, address string, server *PhantomInterface) ([]byte, error) {
	data := make([]byte, 1024)
	binary.BigEndian.PutUint16(data[:2], uint16(len(request)))
	copy(data[2:], request)

	var conn net.Conn
	var err error = nil
	if server != nil {
		host, port := splitHostPort(address)
		conn, _, err = server.Dial(host, port, data[:len(request)+2])
		if err != nil {
			return nil, err
		}
	} else {
		conn, err = net.DialTimeout("tcp", address, time.Second*5)
		if err != nil {
			return nil, err
		}

		_, err = conn.Write(data[:len(request)+2])
		if err != nil {
			conn.Close()
			return nil, err
		}
	}
	defer conn.Close()

	n, err := conn.Read(data)
	if err != nil || n < 2 {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(data[:2]) + 2)
	recvlen := n
	for recvlen < length && n > 0 {
		n, err = conn.Read(data[recvlen:])
		if err != nil {
			return nil, err
		}
		recvlen += n
	}
	return data[2:recvlen], nil
}

func TCPlookupDNS64(request []byte, address string, offset int, prefix []byte) ([]byte, error) {
	response6 := make([]byte, 1024)
	offset6 := offset
	offset4 := offset

	binary.BigEndian.PutUint16(request[offset-4:offset-2], 1)
	response, err := TCPlookup(request, address, nil)
	if err != nil {
		return nil, err
	}

	copy(response6, response[:offset])
	binary.BigEndian.PutUint16(response6[offset-4:offset-2], 28)

	count := int(binary.BigEndian.Uint16(response[6:8]))
	for i := 0; i < count; i++ {
		for {
			if offset >= len(response) {
				log.Println(offset)
				return nil, nil
			}
			length := response[offset]
			offset++
			if length == 0 {
				break
			}
			if length < 63 {
				offset += int(length)
				if offset+2 > len(response) {
					log.Println(offset)
					return nil, nil
				}
			} else {
				offset++
				break
			}
		}
		if offset+2 > len(response) {
			log.Println(offset)
			return nil, nil
		}

		copy(response6[offset6:], response[offset4:offset])
		offset6 += offset - offset4
		offset4 = offset

		AType := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 8
		if offset+2 > len(response) {
			log.Println(offset)
			return nil, nil
		}
		DataLength := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 2

		offset += int(DataLength)
		if AType == 1 {
			if offset > len(response) {
				log.Println(offset)
				return nil, nil
			}
			binary.BigEndian.PutUint16(response6[offset6:], 28)
			offset6 += 2
			offset4 += 2
			copy(response6[offset6:], response[offset4:offset4+6])
			offset6 += 6
			offset4 += 6
			binary.BigEndian.PutUint16(response6[offset6:], DataLength+12)
			offset6 += 2
			offset4 += 2

			copy(response6[offset6:], prefix[:12])
			offset6 += 12
			copy(response6[offset6:], response[offset4:offset])
			offset6 += offset - offset4
			offset4 = offset
		} else {
			copy(response6[offset6:], response[offset4:offset])
			offset6 += offset - offset4
			offset4 = offset
		}
	}

	return response6[:offset6], nil
}

func UDPlookup(request []byte, address string) ([]byte, error) {
	conn, err := net.Dial("udp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_, err = conn.Write(request)
	if err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Now().Add(time.Second * 5))
	response := make([]byte, 1024)

	if request[11] == 0 {
		n, err := conn.Read(response[:])
		return response[:n], err
	} else {
		var n int
		for {
			n, err = conn.Read(response[:])
			if err != nil {
				return nil, err
			}

			if request[11] == 0 || response[11] > 0 {
				break
			}
		}
		return response[:n], nil
	}
}

func TLSlookup(request []byte, address string) ([]byte, error) {
	conf := &tls.Config{
		InsecureSkipVerify: true,
	}
	conn, err := tls.Dial("tcp", address, conf)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	data := make([]byte, 1024)
	binary.BigEndian.PutUint16(data[:2], uint16(len(request)))
	copy(data[2:], request)

	_, err = conn.Write(data[:len(request)+2])
	if err != nil {
		return nil, err
	}

	n, err := conn.Read(data)
	if err != nil || n < 2 {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(data[:2]) + 2)
	recvlen := n
	for recvlen < length && n > 0 {
		n, err = conn.Read(data[recvlen:])
		if err != nil {
			return nil, err
		}
		recvlen += n
	}
	return data[2:recvlen], nil
}

func HTTPSlookup(request []byte, u *url.URL, domain string) ([]byte, error) {
	address := u.Host
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host = address
		port = "443"
		address += ":443"
	}

	if domain == "" {
		domain = host
	}
	path := u.Path
	if port != "443" {
		host = address
	}

	conf := &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         domain,
	}
	conn, err := tls.Dial("tcp", address, conf)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	httpRequest := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\nAccept: application/dns-message\r\nContent-Type: application/dns-message\r\nConnection: close\r\nContent-Length: %d\r\n\r\n", path, host, len(request))
	logPrintln(5, httpRequest)
	_, err = conn.Write([]byte(httpRequest))
	if err != nil {
		return nil, err
	}
	_, err = conn.Write(request)
	if err != nil {
		return nil, err
	}

	data := make([]byte, 2048)
	recvlen := 0
	headerLen := 0
	contentLength := 0
	for {
		n, err := conn.Read(data[recvlen:])
		recvlen += n

		if recvlen > 0 {
			if contentLength == 0 {
				offset := bytes.Index(data[:recvlen], []byte("\r\n\r\n"))
				if offset != -1 {
					headerLen = offset + 4
					header := string(data[:headerLen])
					logPrintln(5, header)
					for _, line := range strings.Split(header, "\r\n") {
						field := strings.SplitN(line, ": ", 2)
						if len(field) > 1 {
							if field[0] == "Content-Length" {
								contentLength, err = strconv.Atoi(field[1])
								continue
							}
						}
					}
				}
			}

			if (recvlen - headerLen) >= contentLength {
				return data[headerLen : headerLen+contentLength], nil
			}
		}

		if err != nil || n == 0 {
			return nil, err
		}
	}
}

func TFOlookup(request []byte, address string) ([]byte, error) {
	data := make([]byte, 1024)
	binary.BigEndian.PutUint16(data[:2], uint16(len(request)))
	copy(data[2:], request)

	var conn net.Conn
	var err error = nil

	addr, err := net.ResolveTCPAddr("tcp", address)
	if err != nil {
		return nil, err
	}
	conn, _, err = DialConnInfo(
		nil, addr,
		&PhantomInterface{
			Hint: OPT_TFO,
			TTL:  1,
		},
		data[:len(request)+2],
	)
	if err != nil {
		return nil, err
	}

	n, err := conn.Read(data)
	if err != nil || n < 2 {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(data[:2]) + 2)
	recvlen := n
	for recvlen < length && n > 0 {
		n, err = conn.Read(data[recvlen:])
		if err != nil {
			return nil, err
		}
		recvlen += n
	}
	return data[2:recvlen], nil
}

func GetQName(buf []byte) (string, int, int) {
	bufflen := len(buf)
	if bufflen < 13 {
		return "", 0, 0
	}
	length := buf[12]
	off := 13
	end := off + int(length)
	qname := string(buf[off:end])
	off = end

	for {
		if off >= bufflen {
			return "", 0, 0
		}
		length := buf[off]
		off++
		if length == 0x00 {
			break
		}
		end := off + int(length)
		if end > bufflen {
			return "", 0, 0
		}
		qname += "." + string(buf[off:end])
		off = end
	}
	end = off + 4
	if end > bufflen {
		return "", 0, 0
	}

	qtype := int(binary.BigEndian.Uint16(buf[off : off+2]))

	return qname, qtype, end
}

func GetName(buf []byte, offset int) (string, int) {
	name := ""
	for {
		length := int(buf[offset])
		offset++
		if length == 0 {
			break
		}
		if name != "" {
			name += "."
		}
		if length < 63 {
			name += string(buf[offset : offset+length])
			offset += int(length)
			if offset+2 > len(buf) {
				return "", offset
			}
		} else {
			_offset := int(buf[offset])
			_name, _ := GetName(buf, _offset)
			name += _name
			return name, offset + 1
		}
	}
	return name, offset
}

func GetNameOffset(response []byte, offset int) int {
	responseLen := len(response)

	for {
		if offset >= responseLen {
			return 0
		}
		length := response[offset]
		offset++
		if length == 0 {
			break
		}
		if length < 63 {
			offset += int(length)
			if offset+2 > responseLen {
				return 0
			}
		} else {
			offset++
			break
		}
	}

	return offset
}

func getAnswers(response []byte) *RecordAddresses {
	responseLen := len(response)

	offset := 12
	if offset > responseLen {
		return nil
	}

	QDCount := int(binary.BigEndian.Uint16(response[4:6]))
	ANCount := int(binary.BigEndian.Uint16(response[6:8]))

	if ANCount == 0 {
		return &RecordAddresses{0, nil}
	}

	for i := 0; i < QDCount; i++ {
		_offset := GetNameOffset(response, offset)
		if _offset == 0 {
			return nil
		}
		offset = _offset + 4
	}

	var ips []net.IP
	var ttl uint32 = 65535
	cname := ""
	for i := 0; i < ANCount; i++ {
		_offset := GetNameOffset(response, offset)
		if _offset == 0 {
			return nil
		}
		offset = _offset
		if offset+2 > responseLen {
			return nil
		}
		AType := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 4
		if offset+4 > responseLen {
			return nil
		}
		TTL := binary.BigEndian.Uint32(response[offset : offset+4])
		if TTL < ttl {
			ttl = TTL
		}
		offset += 4
		if offset+2 > responseLen {
			return nil
		}
		DataLength := binary.BigEndian.Uint16(response[offset : offset+2])
		offset += 2

		if AType == 1 {
			if offset+4 > responseLen {
				return nil
			}
			data := response[offset : offset+4]
			ip := net.IPv4(data[0], data[1], data[2], data[3])
			ips = append(ips, ip)
		} else if AType == 28 {
			var data [16]byte
			if offset+16 > responseLen {
				return nil
			}
			copy(data[:], response[offset:offset+16])
			ip := net.IP(response[offset : offset+16])
			ips = append(ips, ip)
		} else if AType == 5 {
			cname, _ = GetName(response, offset)
			logPrintln(4, "CNAME:", cname)
		}

		offset += int(DataLength)
	}

	//if len(ips) == 0 && cname != "" {
	//	_, ips = NSLookup(cname, qtype)
	//}

	if ttl < DNSMinTTL {
		ttl = DNSMinTTL
	}

	return &RecordAddresses{int64(ttl) + time.Now().Unix(), ips}
}

func packAnswers(qtype int, ttl uint32, records DNSRecords) (int, []byte) {
	totalLen := 0
	count := 0
	var ips []net.IP
	switch qtype {
	case 1:
		if records.A == nil {
			return 0, nil
		}
		ips = records.A.Addresses
	case 28:
		if records.AAAA == nil {
			return 0, nil
		}
		ips = records.AAAA.Addresses
	case 65:
		return 0, nil
	}
	for _, ip := range ips {
		ip4 := ip.To4()
		if ip4 != nil {
			if qtype == 1 {
				count++
				totalLen += 16
			}
		} else {
			if qtype == 28 {
				count++
				totalLen += 28
			}
		}
	}

	answers := make([]byte, totalLen)
	length := 0
	for _, ip := range ips {
		ip4 := ip.To4()
		if ip4 != nil {
			if qtype == 1 {
				copy(answers[length:], []byte{0xC0, 0x0C, 0x00, 1,
					0x00, 0x01})
				length += 6
				binary.BigEndian.PutUint32(answers[length:], ttl)
				length += 4
				copy(answers[length:], []byte{0x00, 0x04})
				length += 2
				copy(answers[length:], ip4)
				length += 4
			}
		} else {
			if qtype == 28 {
				copy(answers[length:], []byte{0xC0, 0x0C, 0x00, 28,
					0x00, 0x01})
				length += 6
				binary.BigEndian.PutUint32(answers[length:], ttl)
				length += 4
				copy(answers[length:], []byte{0x00, 0x10})
				length += 2
				copy(answers[length:], ip)
				length += 16
			}
		}
	}

	return count, answers
}

func (records DNSRecords) BuildResponse(request []byte, qtype int, ttl uint32) []byte {
	response := make([]byte, 1024)
	copy(response, request)
	length := len(request)
	response[2] = 0x81
	response[3] = 0x80

	if records.Index > 0 {
		switch qtype {
		case 1:
			answer := []byte{0xC0, 0x0C, 0x00, 1,
				0x00, 0x01, 0x00, 0x00, 0x00, 0x10, 0x00, 0x04,
				VirtualAddrPrefix, 0}
			copy(response[length:], answer)
			length += 14
			binary.BigEndian.PutUint16(response[length:], uint16(records.Index))
			length += 2
			binary.BigEndian.PutUint16(response[6:], 1)
		case 28:
			return response[:length]

			answer := []byte{0xC0, 0x0C, 0x00, 28,
				0x00, 0x01, 0x00, 0x00, 0x00, 0x10, 0x00, 0x10,
				0x00, 0x64, 0xff, VirtualAddrPrefix, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00}
			copy(response[length:], answer)
			length += 24
			binary.BigEndian.PutUint32(response[length:], uint32(records.Index))
			length += 4
			binary.BigEndian.PutUint16(response[6:], 1)
		case 65:
			copy(response[length:], []byte{0xC0, 0x0C, 0x00, 65, 0, 1, 0, 0, 0, 16, 0, 0, 0, 1, 0})
			dataLenOffset := length + 10
			length += 15
			if records.Hint&(OPT_HTTPS|OPT_HTTP3) != 0 {
				copy(response[length:], []byte{0, 1, 0, 0})
				svcLenOffset := length + 2
				length += 4
				if records.Hint&OPT_HTTP3 != 0 {
					copy(response[length:], []byte{2, 0x68, 0x33})
					length += 3
					copy(response[length:], []byte{5, 0x68, 0x33, 0x2d, 0x32, 0x39})
					length += 6
				}
				if records.Hint&OPT_HTTPS != 0 {
					copy(response[length:], []byte{2, 0x68, 0x32})
					length += 3
				}
				binary.BigEndian.PutUint16(response[svcLenOffset:], uint16(length-svcLenOffset-2))
			}
			copy(response[length:], []byte{0, 4, 0, 4, VirtualAddrPrefix, 0})
			length += 6
			binary.BigEndian.PutUint16(response[length:], uint16(records.Index))
			length += 2
			binary.BigEndian.PutUint16(response[6:], 1)
			binary.BigEndian.PutUint16(response[dataLenOffset:], uint16(length-dataLenOffset-2))
		}
	} else {
		count, answer := packAnswers(qtype, ttl, records)
		if count > 0 {
			binary.BigEndian.PutUint16(response[6:], uint16(count))
			copy(response[length:], answer)
			length += len(answer)
		}
	}

	return response[:length]
}

func PackQName(name string) []byte {
	length := strings.Count(name, "")
	QName := make([]byte, length+1)
	copy(QName[1:], []byte(name))
	o, l := 0, 0
	for i := 1; i < length; i++ {
		if QName[i] == '.' {
			QName[o] = byte(l)
			l = 0
			o = i
		} else {
			l++
		}
	}
	QName[o] = byte(l)

	return QName
}

type ServerOptions struct {
	ECS    string
	Type   string
	PD     string
	Domain string
}

func ParseOptions(options string) ServerOptions {
	opts := strings.Split(options, "&")
	var serverOpts ServerOptions
	for _, opt := range opts {
		key := strings.SplitN(opt, "=", 2)
		if len(key) > 1 {
			switch key[0] {
			case "ecs":
				serverOpts.ECS = key[1]
			case "pd":
				serverOpts.PD = key[1]
			case "type":
				serverOpts.Type = key[1]
			case "domain":
				serverOpts.Domain = key[1]
			}
		}
	}

	return serverOpts
}

func PackRequest(name string, qtype uint16, id uint16, ecs string) []byte {
	Request := make([]byte, 512)

	binary.BigEndian.PutUint16(Request[:], id)      //ID
	binary.BigEndian.PutUint16(Request[2:], 0x0100) //Flag
	binary.BigEndian.PutUint16(Request[4:], 1)      //QDCount
	binary.BigEndian.PutUint16(Request[6:], 0)      //ANCount
	binary.BigEndian.PutUint16(Request[8:], 0)      //NSCount
	if ecs != "" {
		binary.BigEndian.PutUint16(Request[10:], 1) //ARCount
	} else {
		binary.BigEndian.PutUint16(Request[10:], 0) //ARCount
	}

	qname := PackQName(name)
	length := len(qname)
	copy(Request[12:], qname)
	length += 12
	binary.BigEndian.PutUint16(Request[length:], qtype)
	length += 2
	binary.BigEndian.PutUint16(Request[length:], 0x01) //QClass
	length += 2

	if ecs != "" {
		Request[length] = 0 //Name
		length++
		binary.BigEndian.PutUint16(Request[length:], 41) // Type
		length += 2
		binary.BigEndian.PutUint16(Request[length:], 4096) // UDP Payload
		length += 2
		Request[length] = 0 // Highter bits in extended RCCODE
		length++
		Request[length] = 0 // EDNS0 Version
		length++
		binary.BigEndian.PutUint16(Request[length:], 0x800) // Z
		length += 2

		ecsip := net.ParseIP(ecs)
		ecsip4 := ecsip.To4()
		if ecsip4 != nil {
			binary.BigEndian.PutUint16(Request[length:], 11) // Length
			length += 2
			binary.BigEndian.PutUint16(Request[length:], 8) // Option Code
			length += 2
			binary.BigEndian.PutUint16(Request[length:], 7) // Option Length
			length += 2
			binary.BigEndian.PutUint16(Request[length:], 1) // Family
			length += 2
			Request[length] = 24 // Source Netmask
			length++
			Request[length] = 0 // Scope Netmask
			length++
			copy(Request[length:], ecsip4[:3])
			length += 3
		} else {
			binary.BigEndian.PutUint16(Request[length:], 15) // Length
			length += 2
			binary.BigEndian.PutUint16(Request[length:], 8) // Option Code
			length += 2
			binary.BigEndian.PutUint16(Request[length:], 11) // Option Length
			length += 2
			binary.BigEndian.PutUint16(Request[length:], 2) // Family
			length += 2
			Request[length] = 56 // Source Netmask
			length++
			Request[length] = 0 // Scope Netmask
			length++
			copy(Request[length:], ecsip[:7])
			length += 7
		}
	}

	return Request[:length]
}

func LoadDNSCache(qname string) *DNSRecords {
	var ok bool
	var result interface{}
	result, ok = DNSCache.Load(qname)
	if ok {
		return result.(*DNSRecords)
	}

	return nil
}

func StoreDNSCache(qname string, record *DNSRecords) {
	DNSCache.Store(qname, record)
}

func NSLookup(name string, hint uint32, server string) (int, []net.IP) {
	var qtype uint16 = 1
	if hint&OPT_IPV6 != 0 {
		qtype = 28
	}

	records := LoadDNSCache(name)
	if records == nil {
		records = new(DNSRecords)
		StoreDNSCache(name, records)

		offset := 0
		for i := 0; i < SubdomainDepth; i++ {
			off := strings.Index(name[offset:], ".")
			if off == -1 {
				break
			}
			offset += off
			top := LoadDNSCache(name[offset:])
			if top != nil {
				*records = *top
				break
			}

			offset++
		}
	}
	switch qtype {
	case 1:
		if records.A != nil {
			logPrintln(3, "cached:", name, qtype, records.A.Addresses)
			return records.Index, records.A.Addresses
		}
	case 28:
		if records.AAAA != nil {
			logPrintln(3, "cached:", name, qtype, records.AAAA.Addresses)
			return records.Index, records.AAAA.Addresses
		}
	default:
		return 0, nil
	}

	var request []byte
	var response []byte
	var err error

	var options ServerOptions
	u, err := url.Parse(server)
	if err != nil {
		logPrintln(1, err)
		return 0, nil
	}
	if u.RawQuery != "" {
		options = ParseOptions(u.RawQuery)
	}

	if u.Host != "" {
		switch u.Scheme {
		case "udp":
			request = PackRequest(name, qtype, uint16(0), options.ECS)
			response, err = UDPlookup(request, u.Host)
		case "tcp":
			request = PackRequest(name, qtype, uint16(0), options.ECS)
			response, err = TCPlookup(request, u.Host, nil)
		case "tls":
			request = PackRequest(name, qtype, uint16(0), options.ECS)
			response, err = TLSlookup(request, u.Host)
		case "https":
			request = PackRequest(name, qtype, uint16(0), options.ECS)
			response, err = HTTPSlookup(request, u, options.Domain)
		case "tfo":
			request = PackRequest(name, qtype, uint16(0), options.ECS)
			response, err = TFOlookup(request, u.Host)
		default:
			NoseLock.Lock()
			records.Index = len(Nose)
			records.Hint = uint(hint)
			Nose = append(Nose, name)
			NoseLock.Unlock()
			return records.Index, nil
		}
	}
	if err != nil {
		logPrintln(1, err)
		return 0, nil
	}

	if records.Index == 0 && hint != 0 {
		NoseLock.Lock()
		records.Index = len(Nose)
		records.Hint = uint(hint)
		Nose = append(Nose, name)
		NoseLock.Unlock()
	}

	answer := getAnswers(response)
	if answer == nil {
		return records.Index, nil
	}

	if options.PD != "" {
		for i, ip := range answer.Addresses {
			answer.Addresses[i] = net.ParseIP(options.PD + ip.String())
		}
	}
	logPrintln(3, "nslookup", name, qtype, answer.Addresses)
	switch qtype {
	case 1:
		records.A = answer
	case 28:
		records.AAAA = answer
	}

	return records.Index, answer.Addresses
}

func NSRequest(request []byte, cache bool) (int, []byte) {
	name, qtype, _ := GetQName(request)
	if name == "" {
		logPrintln(2, "DNS Segmentation fault")
		return 0, nil
	}

	var records *DNSRecords
	if cache {
		records = LoadDNSCache(name)
		if records == nil {
			records = new(DNSRecords)
			StoreDNSCache(name, records)

			offset := 0
			for i := 0; i < SubdomainDepth; i++ {
				off := strings.Index(name[offset:], ".")
				if off == -1 {
					break
				}
				offset += off
				top := LoadDNSCache(name[offset:])
				if top != nil {
					*records = *top
					break
				}
				offset++
			}
		}
	} else {
		records = new(DNSRecords)
	}

	switch qtype {
	case 1:
		if records.A != nil {
			return records.Index, records.BuildResponse(request, qtype, 0)
		}
	case 28:
		if records.AAAA != nil {
			return records.Index, records.BuildResponse(request, qtype, 0)
		}
	case 65:
		if records.Index == 0 && records.Hint&OPT_UDP != 0 {
			NoseLock.Lock()
			records.Index = len(Nose)
			Nose = append(Nose, name)
			NoseLock.Unlock()
		}
		return records.Index, records.BuildResponse(request, qtype, 3600)
	default:
		return records.Index, records.BuildResponse(request, qtype, 3600)
	}

	var response []byte
	var err error

	server := ConfigLookup(name)
	var options ServerOptions
	DNS := ""
	if server != nil {
		records.Hint = uint(server.Hint)
		logPrintln(2, "request:", name, server.DNS, server.Protocol)
		DNS = server.DNS
	} else {
		logPrintln(4, "request:", name, "no answer")
		return 0, records.BuildResponse(request, qtype, 3600)
	}

	if DNS == "" {
		if records.Index == 0 && server.Protocol != 0 {
			NoseLock.Lock()
			records.Index = len(Nose)
			Nose = append(Nose, name)
			NoseLock.Unlock()
		}
		return records.Index, records.BuildResponse(request, qtype, 3600)
	}

	u, err := url.Parse(DNS)
	if err != nil {
		logPrintln(1, err)
		return 0, nil
	}

	_request := request
	if u.RawQuery != "" {
		options = ParseOptions(u.RawQuery)

		if options.Type == "A" && qtype == 28 {
			return records.Index, records.BuildResponse(request, qtype, 0)
		} else if options.Type == "AAAA" && qtype == 1 {
			return records.Index, records.BuildResponse(request, qtype, 0)
		}

		_qtype := uint16(qtype)
		if records.Hint&OPT_IPV6 != 0 {
			_qtype = 28
		}

		if options.ECS != "" || _qtype != uint16(qtype) {
			id := binary.BigEndian.Uint16(request[:2])
			_request = PackRequest(name, _qtype, id, options.ECS)
		}
	}

	switch u.Scheme {
	case "udp":
		response, err = UDPlookup(_request, u.Host)
	case "tcp":
		response, err = TCPlookup(_request, u.Host, nil)
	case "tls":
		response, err = TLSlookup(_request, u.Host)
	case "https":
		response, err = HTTPSlookup(_request, u, options.Domain)
	case "tfo":
		response, err = TFOlookup(_request, u.Host)
	default:
		logPrintln(1, "unknown protocol")
		return 0, nil
	}

	if err != nil {
		logPrintln(1, err)
		return 0, nil
	}

	if records.Index == 0 && (records.Hint != 0 || server.Protocol != 0) {
		NoseLock.Lock()
		records.Index = len(Nose)
		Nose = append(Nose, name)
		NoseLock.Unlock()
	}
	answer := getAnswers(response)
	if answer == nil {
		logPrintln(4, "request:", name, "no answer")
		return 0, records.BuildResponse(request, qtype, 0)
	}

	if options.PD != "" {
		for i, ip := range answer.Addresses {
			answer.Addresses[i] = net.ParseIP(options.PD + ip.String())
		}
	}
	logPrintln(3, "response:", name, qtype, answer.Addresses)
	switch qtype {
	case 1:
		records.A = answer
	case 28:
		records.AAAA = answer
	}

	return records.Index, records.BuildResponse(request, qtype, 0)
}

func (server *PhantomInterface) ResolveTCPAddr(host string, port int) (*net.TCPAddr, error) {
	ip := net.ParseIP(host)
	if ip != nil {
		return &net.TCPAddr{IP: ip, Port: port}, nil
	}

	_, addrs := NSLookup(host, server.Hint, server.DNS)
	if len(addrs) == 0 {
		return nil, errors.New("no such host")
	}
	rand.Seed(time.Now().UnixNano())
	return &net.TCPAddr{IP: addrs[rand.Intn(len(addrs))], Port: port}, nil
}

func (server *PhantomInterface) ResolveTCPAddrs(host string, port int) ([]*net.TCPAddr, error) {
	ip := net.ParseIP(host)
	if ip != nil {
		tcpAddrs := make([]*net.TCPAddr, 1)
		tcpAddrs[0] = &net.TCPAddr{IP: ip, Port: port}
		return tcpAddrs, nil
	}

	_, addrs := NSLookup(host, server.Hint, server.DNS)
	if len(addrs) == 0 {
		return nil, errors.New("no such host")
	}
	tcpAddrs := make([]*net.TCPAddr, len(addrs))
	for i, addr := range addrs {
		tcpAddrs[i] = &net.TCPAddr{IP: addr, Port: port}
	}

	return tcpAddrs, nil
}

func DNSTCPServer(client net.Conn) {
	defer client.Close()

	data := make([]byte, 2048)
	n, err := client.Read(data)
	if err != nil {
		return
	}
	requestLen := int(binary.BigEndian.Uint16(data[:2]))
	if requestLen > (n - 2) {
		return
	}
	request := make([]byte, requestLen)
	copy(request, data[2:requestLen-2])

	_, response := NSRequest(request, true)
	responseLen := len(response)
	binary.BigEndian.PutUint16(data[:2], uint16(responseLen))
	copy(data[2:], response)
	client.Write(data[:responseLen+2])
}

// +build linux
// +build mipsle

package phantomtcp

import (
	"fmt"
	"net"
	"sync"
	"syscall"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
)

func DevicePrint() {
}

type ConnectionInfo struct {
	Link gopacket.LinkLayer
	IP   gopacket.NetworkLayer
	TCP  layers.TCP
}

var SynLock sync.Mutex
var ConnSyn map[string]int
var ConnInfo4 [65536]*ConnectionInfo
var ConnInfo6 [65536]*ConnectionInfo

func connectionMonitor(device string, ipv6 bool) {
	fmt.Printf("Device: %v\n", device)

	var err error
	localaddr, err := GetLocalAddr(device, ipv6)
	if err != nil {
		logPrintln(1, err)
		return
	}

	var handle *net.IPConn
	if ipv6 {
		netaddr, _ := net.ResolveIPAddr("ip6", localaddr.IP.String())
		handle, err = net.ListenIP("ip6:tcp", netaddr)
	} else {
		netaddr, _ := net.ResolveIPAddr("ip4", localaddr.IP.String())
		handle, err = net.ListenIP("ip4:tcp", netaddr)
	}

	if err != nil {
		fmt.Printf("sockraw open failed: %v", err)
		return
	}
	defer handle.Close()

	buf := make([]byte, 1500)
	for {
		n, addr, err := handle.ReadFrom(buf)
		if err != nil {
			logPrintln(1, err)
			continue
		}

		var tcp layers.TCP

		tcp.DecodeFromBytes(buf[:n], nil)

		if tcp.SYN != true {
			continue
		}
		srcPort := tcp.DstPort
		synAddr := addr.String()
		SynLock.Lock()
		_, ok := ConnSyn[synAddr]
		if ok {
			if ipv6 {
				var ip layers.IPv6
				ip.Version = 6
				ip.TrafficClass = 5
				ip.FlowLabel = 0
				ip.Length = 0
				ip.NextHeader = 6
				ip.HopLimit = 64
				ip.SrcIP = localaddr.IP
				ip.DstIP = net.ParseIP(addr.String())
				ip.HopByHop = nil

				tcp.DstPort = tcp.SrcPort
				tcp.SrcPort = srcPort
				ack := tcp.Seq + 1
				tcp.Seq = tcp.Ack
				tcp.Ack = ack

				ConnInfo4[srcPort] = &ConnectionInfo{nil, &ip, tcp}
				buf = make([]byte, 1500)
			} else {
				var ip layers.IPv4
				ip.Version = 4
				ip.IHL = 5
				ip.TOS = 0
				ip.Length = 0
				ip.Id = 0
				ip.Flags = 0
				ip.FragOffset = 0
				ip.TTL = 64
				ip.Protocol = 6
				ip.Checksum = 0
				ip.SrcIP = localaddr.IP
				ip.DstIP = net.ParseIP(addr.String())
				ip.Options = nil
				ip.Padding = nil

				tcp.DstPort = tcp.SrcPort
				tcp.SrcPort = srcPort
				ack := tcp.Seq + 1
				tcp.Seq = tcp.Ack
				tcp.Ack = ack

				ConnInfo4[srcPort] = &ConnectionInfo{nil, &ip, tcp}
				buf = make([]byte, 1500)
			}
		}
		SynLock.Unlock()
	}
}

func ConnectionMonitor(devices []string) {
	if len(devices) == 1 {
		go connectionMonitor(devices[0], true)
		connectionMonitor(devices[0], false)
	} else {
		for i := 1; i < len(devices); i++ {
			go connectionMonitor(devices[i], true)
			go connectionMonitor(devices[i], false)
		}
		go connectionMonitor(devices[0], true)
		connectionMonitor(devices[0], false)
	}
}

func SendFakePacket(connInfo *ConnectionInfo, payload []byte, config *Config, count int) error {
	ipLayer := connInfo.IP

	tcpLayer := &layers.TCP{
		SrcPort:    connInfo.TCP.SrcPort,
		DstPort:    connInfo.TCP.DstPort,
		Seq:        connInfo.TCP.Seq,
		Ack:        connInfo.TCP.Ack,
		DataOffset: 5,
		ACK:        true,
		PSH:        true,
		Window:     connInfo.TCP.Window,
	}

	if config.Option&OPT_WMD5 != 0 {
		tcpLayer.Options = []layers.TCPOption{
			layers.TCPOption{19, 18, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}},
		}
	}

	if config.Option&OPT_NACK != 0 {
		tcpLayer.ACK = false
		tcpLayer.Ack = 0
	} else if config.Option&OPT_WACK != 0 {
		tcpLayer.Ack += uint32(tcpLayer.Window)
	}

	// And create the packet with the layers
	buffer := gopacket.NewSerializeBuffer()
	var options gopacket.SerializeOptions
	options.FixLengths = true

	if config.Option&OPT_WCSUM == 0 {
		options.ComputeChecksums = true
	}

	tcpLayer.SetNetworkLayerForChecksum(ipLayer)
	var sa syscall.Sockaddr
	var domain int

	switch ip := ipLayer.(type) {
	case *layers.IPv4:
		if config.Option&OPT_TTL != 0 {
			ip.TTL = config.TTL
		}
		gopacket.SerializeLayers(buffer, options,
			ip, tcpLayer, gopacket.Payload(payload),
		)
		var addr [4]byte
		copy(addr[:4], ip.DstIP.To4()[:4])
		sa = &syscall.SockaddrInet4{Addr: addr, Port: 0}
		domain = syscall.AF_INET
	case *layers.IPv6:
		if config.Option&OPT_TTL != 0 {
			ip.HopLimit = config.TTL
		}
		gopacket.SerializeLayers(buffer, options,
			ip, tcpLayer, gopacket.Payload(payload),
		)
		var addr [16]byte
		copy(addr[:16], ip.DstIP[:16])
		sa = &syscall.SockaddrInet6{Addr: addr, Port: 0}
		domain = syscall.AF_INET6
	}

	raw_fd, err := syscall.Socket(domain, syscall.SOCK_RAW, syscall.IPPROTO_RAW)
	if err != nil {
		syscall.Close(raw_fd)
		return err
	}
	outgoingPacket := buffer.Bytes()

	for i := 0; i < count; i++ {
		err = syscall.Sendto(raw_fd, outgoingPacket, 0, sa)
		if err != nil {
			syscall.Close(raw_fd)
			return err
		}
	}
	syscall.Close(raw_fd)

	return nil
}
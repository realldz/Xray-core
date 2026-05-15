package singmux

import (
	"encoding/binary"
	"fmt"
	"io"
	"strconv"

	"github.com/xtls/xray-core/common/net"
)

const (
	ProtocolSmux  = 0
	ProtocolYAMux = 1
	ProtocolH2Mux = 2
)

const (
	Version0 = 0
	Version1 = 1
)

const (
	flagUDP       = 0x0001
	flagAddr      = 0x0002
	statusSuccess = 0x00
	statusError   = 0x01
)

var SingMuxAddr = net.TCPDestination(net.DomainAddress("sp.mux.sing-box.arpa"), net.Port(444))

func IsSingMuxAddress(dest net.Destination) bool {
	return dest.Network == net.Network_TCP &&
		dest.Address.Family().IsDomain() &&
		dest.Address.Domain() == "sp.mux.sing-box.arpa" &&
		dest.Port == net.Port(444)
}

type HandshakeRequest struct {
	Version  byte
	Protocol byte
	Padding  bool
}

func ReadHandshake(reader io.Reader) (*HandshakeRequest, error) {
	var version byte
	err := binary.Read(reader, binary.BigEndian, &version)
	if err != nil {
		return nil, err
	}
	if version != Version0 {
		return nil, fmt.Errorf("unsupported sing-mux version: %d", version)
	}
	var protocol byte
	err = binary.Read(reader, binary.BigEndian, &protocol)
	if err != nil {
		return nil, err
	}
	return &HandshakeRequest{Version: version, Protocol: protocol}, nil
}

func WriteHandshake(writer io.Writer, req *HandshakeRequest) error {
	_, err := writer.Write([]byte{req.Version, req.Protocol})
	return err
}

const (
	addrTypeIPv4 = 0x01
	addrTypeIPv6 = 0x04
	addrTypeFqdn = 0x03
)

func WriteSocksAddr(writer io.Writer, dest net.Destination) error {
	var addrType byte

	if dest.Address.Family().IsIPv4() {
		addrType = addrTypeIPv4
	} else if dest.Address.Family().IsIPv6() {
		addrType = addrTypeIPv6
	} else {
		addrType = addrTypeFqdn
	}

	err := binary.Write(writer, binary.BigEndian, addrType)
	if err != nil {
		return err
	}

	switch addrType {
	case addrTypeIPv4:
		ip := dest.Address.IP().To4()
		_, err = writer.Write(ip)
	case addrTypeIPv6:
		ip := dest.Address.IP().To16()
		_, err = writer.Write(ip)
	case addrTypeFqdn:
		domain := dest.Address.Domain()
		if len(domain) > 255 {
			return fmt.Errorf("domain too long: %d", len(domain))
		}
		err = binary.Write(writer, binary.BigEndian, byte(len(domain)))
		if err != nil {
			return err
		}
		_, err = writer.Write([]byte(domain))
	}
	if err != nil {
		return err
	}

	return binary.Write(writer, binary.BigEndian, dest.Port)
}

func ReadSocksAddr(reader io.Reader) (net.Destination, error) {
	var addrType byte
	err := binary.Read(reader, binary.BigEndian, &addrType)
	if err != nil {
		return net.Destination{}, err
	}

	var address net.Address
	switch addrType {
	case addrTypeIPv4:
		var ip [4]byte
		_, err = io.ReadFull(reader, ip[:])
		if err != nil {
			return net.Destination{}, err
		}
		address = net.IPAddress(net.IP(ip[:]))
	case addrTypeIPv6:
		var ip [16]byte
		_, err = io.ReadFull(reader, ip[:])
		if err != nil {
			return net.Destination{}, err
		}
		address = net.IPAddress(net.IP(ip[:]))
	case addrTypeFqdn:
		var strLen byte
		err = binary.Read(reader, binary.BigEndian, &strLen)
		if err != nil {
			return net.Destination{}, err
		}
		strBytes := make([]byte, strLen)
		_, err = io.ReadFull(reader, strBytes)
		if err != nil {
			return net.Destination{}, err
		}
		address = net.DomainAddress(string(strBytes))
	default:
		return net.Destination{}, fmt.Errorf("unknown address type: 0x%02x", addrType)
	}

	var port net.Port
	err = binary.Read(reader, binary.BigEndian, &port)
	if err != nil {
		return net.Destination{}, err
	}

	return net.TCPDestination(address, port), nil
}

func WriteStreamRequest(writer io.Writer, dest net.Destination) error {
	var flags uint16
	_, err := writer.Write([]byte{byte(flags >> 8), byte(flags & 0xff)})
	if err != nil {
		return err
	}
	return WriteSocksAddr(writer, dest)
}

func ReadStreamRequest(reader io.Reader) (net.Destination, error) {
	var flags uint16
	err := binary.Read(reader, binary.BigEndian, &flags)
	if err != nil {
		return net.Destination{}, err
	}
	dest, err := ReadSocksAddr(reader)
	if err != nil {
		return net.Destination{}, err
	}
	if flags&flagUDP != 0 {
		dest.Network = net.Network_UDP
	}
	return dest, nil
}

func WriteStreamResponse(writer io.Writer) error {
	_, err := writer.Write([]byte{statusSuccess})
	return err
}

func ReadStreamResponse(reader io.Reader) error {
	var status byte
	err := binary.Read(reader, binary.BigEndian, &status)
	if err != nil {
		return err
	}
	if status == statusError {
		var msgLen byte
		err = binary.Read(reader, binary.BigEndian, &msgLen)
		if err != nil {
			return err
		}
		msg := make([]byte, msgLen)
		_, err = io.ReadFull(reader, msg)
		if err != nil {
			return err
		}
		return fmt.Errorf("multiplex error: %s", string(msg))
	}
	return nil
}

type singMuxAddr struct {
	network string
	addr    string
}

func (a *singMuxAddr) Network() string { return a.network }
func (a *singMuxAddr) String() string  { return a.addr }

func netAddr(dest net.Destination) net.Addr {
	return &singMuxAddr{
		network: "tcp",
		addr:    net.JoinHostPort(dest.Address.String(), strconv.Itoa(int(dest.Port))),
	}
}

// +build windows

package netroute

// Implementation Warning: mapping of the correct interface ID and index is not
// hooked up.
// Reference:
// https://docs.microsoft.com/en-us/windows/win32/api/netioapi/nf-netioapi-getbestroute2
import (
	"encoding/binary"
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"github.com/google/gopacket/routing"
	sockaddrconv "github.com/libp2p/go-sockaddr"
	sockaddrnet "github.com/libp2p/go-sockaddr/net"
	"golang.org/x/sys/windows"
)

var (
	modiphlpapi = syscall.NewLazyDLL("iphlpapi.dll")

	procGetBestRoute2 = modiphlpapi.NewProc("GetBestRoute2")
)

type NetLUID uint64

type AddressPrefix struct {
	*windows.RawSockaddrAny
	PrefixLength byte
}

type RouteProtocol uint32 // MIB_IPFORWARD_PROTO

// https://docs.microsoft.com/en-us/windows/win32/api/netioapi/ns-netioapi-mib_ipforward_row2
type mib_row2 struct {
	luid              NetLUID
	index             uint32
	destinationPrefix *AddressPrefix
	nextHop           *windows.RawSockaddrAny
	prefixLength      byte
	lifetime          uint32
	preferredLifetime uint32
	metric            uint32
	protocol          RouteProtocol
	loopback          byte
	autoconfigured    byte
	publish           byte
	immortal          byte
	age               uint32
	origin            byte
}

func callBestRoute(source, dest net.IP) (*mib_row2, net.IP, error) {
	sourceAddr, _, _ := sockaddrconv.SockaddrToAny(sockaddrnet.IPAndZoneToSockaddr(source, ""))
	destAddr, _, _ := sockaddrconv.SockaddrToAny(sockaddrnet.IPAndZoneToSockaddr(dest, ""))
	bestRoute := make([]byte, 512)
	bestSource := make([]byte, 116)

	err := getBestRoute2(nil, 0, sourceAddr, destAddr, 0, bestRoute, bestSource)
	if err != nil {
		return nil, nil, err
	}

	// interpret best route and best source.
	route, err := parseRoute(bestRoute)
	if err != nil {
		return nil, nil, err
	}

	var bestSourceRaw windows.RawSockaddrAny
	bestSourceRaw.Addr.Family = binary.LittleEndian.Uint16(bestSource[0:2])
	copyInto(bestSourceRaw.Addr.Data[:], bestSource[2:16])
	copyInto(bestSourceRaw.Pad[:], bestSource[16:])
	addr, _ := bestSourceRaw.Sockaddr()
	bestSrc, _ := sockaddrnet.SockaddrToIPAndZone(addr)

	return route, bestSrc, nil
}

func copyInto(dst []int8, src []byte) {
	for i, b := range src {
		dst[i] = int8(b)
	}
}

func parseRoute(mib []byte) (*mib_row2, error) {
	var route mib_row2
	var err error

	route.luid = NetLUID(binary.LittleEndian.Uint64(mib[0:]))
	route.index = binary.LittleEndian.Uint32(mib[8:])
	idx := 0
	route.destinationPrefix, idx, err = readDestPrefix(mib, 12)
	if err != nil {
		return nil, err
	}
	route.nextHop, idx, err = readSockAddr(mib, idx)
	if err != nil {
		return nil, err
	}
	route.prefixLength = mib[idx]
	idx += 1
	route.lifetime = binary.LittleEndian.Uint32(mib[idx : idx+4])
	idx += 4
	route.preferredLifetime = binary.LittleEndian.Uint32(mib[idx : idx+4])
	idx += 4
	route.metric = binary.LittleEndian.Uint32(mib[idx : idx+4])
	idx += 4
	route.protocol = RouteProtocol(binary.LittleEndian.Uint32(mib[idx : idx+4]))
	idx += 4
	route.loopback = mib[idx]
	idx += 1
	route.autoconfigured = mib[idx]
	idx += 1
	route.publish = mib[idx]
	idx += 1
	route.immortal = mib[idx]
	idx += 1
	route.age = binary.LittleEndian.Uint32(mib[idx : idx+4])
	idx += 4
	route.origin = mib[idx]

	return &route, err
}

func readDestPrefix(buffer []byte, idx int) (*AddressPrefix, int, error) {
	sock, idx, err := readSockAddr(buffer, idx)
	if err != nil {
		return nil, 0, err
	}
	pfixLen := buffer[idx]
	return &AddressPrefix{sock, pfixLen}, idx + 1, nil
}

func readSockAddr(buffer []byte, idx int) (*windows.RawSockaddrAny, int, error) {
	var rsa windows.RawSockaddrAny
	rsa.Addr.Family = binary.LittleEndian.Uint16(buffer[idx : idx+2])
	if rsa.Addr.Family == 2 /* AF_INET */ || rsa.Addr.Family == 0 /* AF_UNDEF */ {
		copyInto(rsa.Addr.Data[:], buffer[idx+2:idx+16])
		return &rsa, idx + 16, nil
	} else if rsa.Addr.Family == 23 /* AF_INET6 */ {
		//TODO: 24 bytes?
		panic("no v6 len")
	} else {
		return nil, 0, fmt.Errorf("Unknown windows addr family %d", rsa.Addr.Family)
	}
}

func getBestRoute2(interfaceLuid *NetLUID, interfaceIndex uint32, sourceAddress, destinationAddress *windows.RawSockaddrAny, addressSortOptions uint32, bestRoute []byte, bestSourceAddress []byte) (errcode error) {
	r0, _, _ := syscall.Syscall9(procGetBestRoute2.Addr(), 7,
		uintptr(unsafe.Pointer(interfaceLuid)),
		uintptr(interfaceIndex),
		uintptr(unsafe.Pointer(sourceAddress)),
		uintptr(unsafe.Pointer(destinationAddress)),
		uintptr(addressSortOptions),
		uintptr(unsafe.Pointer(&bestRoute[0])),
		uintptr(unsafe.Pointer(&bestSourceAddress[0])),
		0, 0)
	if r0 != 0 {
		errcode = syscall.Errno(r0)
	}
	return
}

type winRouter struct{}

func (r *winRouter) Route(dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	return r.RouteWithSrc(nil, nil, dst)
}

func (r *winRouter) RouteWithSrc(input net.HardwareAddr, src, dst net.IP) (iface *net.Interface, gateway, preferredSrc net.IP, err error) {
	route, pref, err := callBestRoute(src, dst)
	if err != nil {
		return nil, nil, nil, err
	}
	if route.nextHop.Addr.Family == 0 /* AF_UNDEF */ {
		route.nextHop.Addr.Family = 2
		return nil, nil, pref, nil
	}
	addr, err := route.nextHop.Sockaddr()
	if err != nil {
		return nil, nil, nil, err
	}
	nextHop, _ := sockaddrnet.SockaddrToIPAndZone(addr)

	return nil, nextHop, pref, nil
}

func New() (routing.Router, error) {
	rtr := &winRouter{}
	return rtr, nil
}

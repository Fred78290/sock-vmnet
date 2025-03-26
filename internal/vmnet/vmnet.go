// nolint:gocritic,exhaustivestruct,exhaustruct,nosnakecase
package vmnet

// #cgo LDFLAGS: -framework vmnet
// #include "vmnet.h"
import "C"

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

	"github.com/rs/zerolog/log"
	"inet.af/netaddr"
)

var (
	errUnspecifiedFailure      = errors.New("vmnet: unspecified failure")
	errOutOfMemory             = errors.New("vmnet: out of memory")
	errInvalidArgument         = errors.New("vmnet: invalid argument provided")
	errSetupIncomplete         = errors.New("vmnet: interface setup is incomplete")
	errPermissionDenied        = errors.New("vmnet: permission denied. Is the process running as root?")
	errPacketSizeLargerThanMTU = errors.New("vmnet: larger packet size than MTU")
	errKernelBufferExhausted   = errors.New("vmnet: kernel buffer exhausted")
	errTooManyPackets          = errors.New("vmnet: too many packets")
	errSharingServiceBusy      = errors.New("vmnet: sharing service busy")
	errNotAuthorized           = errors.New("vmnet: not authorized")
	errNotWritten              = errors.New("vmnet: packet not written")
	errSetupCallback           = errors.New("vmnet: could not setup callback")
	errNoPackageRead           = errors.New("vmnet: no package read")
)

const successCode = 1000

var errCodesMap = map[int]error{
	1001: errUnspecifiedFailure,
	1002: errOutOfMemory,
	1003: errInvalidArgument,
	1004: errSetupIncomplete,
	1005: errPermissionDenied,
	1006: errPacketSizeLargerThanMTU,
	1007: errKernelBufferExhausted,
	1008: errTooManyPackets,
	1009: errSharingServiceBusy,
	1010: errNotAuthorized,
	2001: errNotWritten,
	3000: errSetupCallback,
	4000: errNoPackageRead,
}

func maptoErr(code int) error {
	err, ok := errCodesMap[code]
	if !ok {
		return errUnspecifiedFailure
	}
	return err
}

type OperationMode C.uint32_t

// https://developer.apple.com/documentation/vmnet/operating_modes_t
const (
	// Host: not implemented
	Host OperationMode = 1000
	// Shared good old NAT
	Shared OperationMode = 1001
	// Bridged: not implemented
	Bridged OperationMode = 1002
)

type IsolationMode bool

// If enabled, no VM <-> VM communication allowed
// https://developer.apple.com/documentation/vmnet/vmnet_enable_isolation_key
const (
	// NOTE: might want to let the user define it via flags
	Enabled  IsolationMode = true
	Disabled IsolationMode = false
)

// Wee need to pass this global variable through the C realm of vmnet,
// so that we can access fields & functions of the VMNet struct from packetsAvailable func.
//
// Read more: https://eli.thegreenplace.net/2019/passing-callbacks-and-pointers-to-cgo/
// We could use something like this instead: https://github.com/mattn/go-pointer
var vmnetPtr *VMNet

type Params struct {
	Mode             OperationMode
	NetworkInterface string
	InterfaceID      string
	NAT66Prefix      string
	PIDFile          string
	StartAddr        netaddr.IP
	EndAddr          netaddr.IP
	SubnetMask       netaddr.IP
	Debug            bool
}

type VMNet struct {
	// vmnet params
	Params

	// The maximum size of the packet that can be written to the interface.
	// This also defines the minimum size of the packet that needs to be passed
	// to the vmnet function for a successful read.
	MaxPacketSize int
	// The MTU to be configured on the virtual interface in the guest operating system.
	MTU int
	// By listening on VMNET_INTERFACE_PACKETS_AVAILABLE events, the registered callback
	// notifes us that the interface is readable. The read packes are being passed to the Even chan.
	// See packetsAvailable for more.
	Event chan []byte

	// CGO representation of the VMNet interface
	iface C.interface_ref
	// CGO representation of max packet size
	mps C.ulonglong
	// CGO representation of mtu
	mtu C.ulonglong
}

func New(p Params) *VMNet {
	return &VMNet{
		Params: p,
		// I found the 100 buffer size to be optimal performance wise
		Event: make(chan []byte, 100),
	}
}

func (v *VMNet) Start() error {
	bridgedInterface := C.CString(v.NetworkInterface)
	interfaceID := C.CString(v.InterfaceID)
	nat66prefix := C.CString(v.NAT66Prefix)
	startAddr := C.CString(v.StartAddr.String())
	endAddr := C.CString(v.EndAddr.String())
	subnetMask := C.CString(v.SubnetMask.String())

	defer C.free(unsafe.Pointer(bridgedInterface))
	defer C.free(unsafe.Pointer(interfaceID))
	defer C.free(unsafe.Pointer(nat66prefix))

	defer C.free(unsafe.Pointer(startAddr))
	defer C.free(unsafe.Pointer(endAddr))
	defer C.free(unsafe.Pointer(subnetMask))

	// Create the interface. From this point, ifconfig will show both bridge100 and vmenet<n> interfaces.
	errCode := C._vmnet_start(&v.iface,
		bridgedInterface,
		interfaceID,
		&v.mps,
		&v.mtu,
		startAddr,
		endAddr,
		subnetMask,
		nat66prefix,
		C.uint32_t(v.Mode),
		C.bool(Enabled),
		C.bool(v.Debug))

	if errCode != successCode || v.iface == nil {
		return maptoErr(int(errCode))
	}

	// Read and save actual max packet size and mtu values from the interface config
	v.MaxPacketSize = int(v.mps)
	v.MTU = int(v.mtu)

	// set the global pointer to the current state of self
	vmnetPtr = v
	// Write the PID file
	pid := os.Getpid()
	file, err := os.Create(v.PIDFile)
	if err != nil {
		return errors.New("failed to create PID file: " + err.Error())
	}

	defer file.Close()

	_, err = file.WriteString(fmt.Sprintf("%d\n", pid))
	if err != nil {
		return errors.New("failed to write to PID file: " + err.Error())
	}

	if err := os.Chmod(v.PIDFile, 0644); err != nil {
		return errors.New("failed to set permissions on PID file: " + err.Error())
	}

	return nil
}

func (v *VMNet) Stop() error {
	defer close(v.Event)
	if errCode := C._vmnet_stop(v.iface); errCode != successCode {
		return maptoErr(int(errCode))
	}

	os.Remove(v.PIDFile)

	return nil
}

func (v *VMNet) read() ([]byte, error) {
	var cBytes unsafe.Pointer
	var cBytesLen C.ulong

	// The C code performs the memory deallocation of cBytes, no need to call C.free here.
	if errCode := C._vmnet_read(v.iface, v.mps, &cBytes, &cBytesLen); errCode != successCode {
		return nil, maptoErr(int(errCode))
	}
	return C.GoBytes(cBytes, C.int(cBytesLen)), nil
}

func (v *VMNet) Write(p []byte) (int, error) {
	// The C code performs the memory deallocation of cBytes, no need to call C.free here.
	if errCode := C._vmnet_write(v.iface, C.CBytes(p), C.ulong(len(p))); errCode != successCode {
		return 0, maptoErr(int(errCode))
	}
	return len(p), nil
}

type EventType uint32

const (
	packetAvailableEvent EventType = 1 << 0
)

//export packetsAvailable
func packetsAvailable(eventType uint32, pckAvailable uint64) {
	// VMNet tells us how many packages we can expect to be able to read from the interface.
	if EventType(eventType) == packetAvailableEvent {
		for i := 0; i < int(pckAvailable); i++ {
			bytes, err := vmnetPtr.read()
			if err != nil {
				log.Error().Err(err).Msg("reading vmnet")
				// go about our bussiness
			}
			vmnetPtr.Event <- bytes
		}
	}
}

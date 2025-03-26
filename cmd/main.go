package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/nagypeterjob/sock-vmnet/internal/stack"
	"github.com/nagypeterjob/sock-vmnet/internal/vmnet"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"inet.af/netaddr"
)

func main() {
	ctx := newCancelableContext()

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	if err := run(ctx); err != nil {
		log.Error().Err(err).Msg("running network stack")
		os.Exit(1)
	}

	<-ctx.Done()
}

func run(ctx context.Context) error {
	var fd string
	var inf string
	var mode string
	var macAddr string
	var startAddr string
	var endAddr string
	var subnetMask string
	var interfaceID string = uuid.New().String()
	var nat66Prefix string
	var pidFile string
	var logLevel string = "info"
	var debug bool
	var networkMode vmnet.OperationMode

	arguments := make([]string, 0, len(os.Args))

	for _, arg := range os.Args[1:] {
		if strings.HasPrefix(arg, "--") {
			arguments = append(arguments, arg[1:])
		} else {
			arguments = append(arguments, arg)
		}
	}

	flag.StringVar(&logLevel, "log-level", "", "Log level (debug, info, warn, error, fatal, panic)")
	flag.StringVar(&fd, "fd", "", "Use file descriptor for VMNet")
	flag.StringVar(&mode, "mode", "bridged", "vmnet mode")
	flag.StringVar(&inf, "interface", "en0", "interface used for --vmnet=bridged, e.g., \"en0\"")
	flag.StringVar(&macAddr, "mac-address", "", "Mac address configured for VM network interface")
	flag.StringVar(&startAddr, "gateway", "192.168.64.1", "gateway used for --vmnet=(host|shared), e.g., \"192.168.64.1\"")
	flag.StringVar(&endAddr, "dhcp-end", "192.168.64.254", "end of the DHCP range")
	flag.StringVar(&subnetMask, "netmask", "255.255.255.0", "requires --gateway to be specified")
	flag.StringVar(&interfaceID, "interface-id", "", "randomly generated if not specified")
	flag.StringVar(&nat66Prefix, "nat66-prefix", "", "The IPv6 prefix to use with shared mode")
	flag.StringVar(&pidFile, "pidfile", "/var/run/vmnet.pid", "save pid to PIDFILE")
	flag.BoolVar(&debug, "debug", false, "Debug vmnet")

	//flag.CommandLine.Parse(arguments)
	flag.Parse()

	fmt.Println("arguments: ", arguments)
	fmt.Println("logLevel: ", logLevel)
	fmt.Println("fd: ", fd)
	fmt.Println("mode: ", mode)
	fmt.Println("inf: ", inf)
	fmt.Println("macAddr: ", macAddr)
	fmt.Println("startAddr: ", startAddr)
	fmt.Println("endAddr: ", endAddr)
	fmt.Println("subnetMask: ", subnetMask)
	fmt.Println("interfaceID: ", interfaceID)
	fmt.Println("nat66Prefix: ", nat66Prefix)
	fmt.Println("pidFile: ", pidFile)
	fmt.Println("debug: ", debug)

	switch mode {
	case "bridged":
		networkMode = vmnet.Bridged
	case "host":
		networkMode = vmnet.Host
	case "shared":
		networkMode = vmnet.Shared
	default:
		return fmt.Errorf("invalid network mode: %s", mode)
	}

	if level, err := zerolog.ParseLevel(logLevel); err != nil {
		return fmt.Errorf("invalid log level: %s", logLevel)
	} else {
		zerolog.SetGlobalLevel(level)
	}

	if pidFile == "" {
		return fmt.Errorf("pidfile is required")
	}

	fdInt, err := strconv.Atoi(fd)
	if err != nil {
		return fmt.Errorf("parsing file descriptor (%s): %w", fd, err)
	}

	log.Debug().Msgf("VM MAC address: %s", macAddr)

	hardwareAddr, err := net.ParseMAC(macAddr)
	if err != nil {
		return fmt.Errorf("parsing provided MAC address: %w", err)
	}

	st, err := stack.NewNetwork(stack.NetworkParams{
		Fd:               fdInt,
		NetworkMode:      networkMode,
		NetworkInterface: inf,
		InterfaceID:      interfaceID,
		Nat66Prefix:      nat66Prefix,
		HardwareAddr:     hardwareAddr,
		StartAddr:        netaddr.MustParseIP(startAddr),
		EndAddr:          netaddr.MustParseIP(endAddr),
		SubnetMask:       netaddr.MustParseIP(subnetMask),
		Debug:            debug,
		PIDFile:          pidFile,
	})

	if err != nil {
		return fmt.Errorf("creating proxy: %w", err)
	}

	if err := st.Run(ctx); err != nil {
		return fmt.Errorf("running proxy: %w", err)
	}

	return nil
}

// exit on signal.
func newCancelableContext() context.Context {
	doneCh := make(chan os.Signal, 1)
	signal.Notify(doneCh, os.Interrupt)

	ctx := context.Background()
	ctx, cancel := context.WithCancel(ctx)

	go func() {
		<-doneCh
		log.Info().Msg("signal received")
		cancel()
	}()

	return ctx
}

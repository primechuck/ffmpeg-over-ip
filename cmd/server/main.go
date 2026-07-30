package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/steelbrain/ffmpeg-over-ip/internal/auth"
	"github.com/steelbrain/ffmpeg-over-ip/internal/config"
	"github.com/steelbrain/ffmpeg-over-ip/internal/process"
	"github.com/steelbrain/ffmpeg-over-ip/internal/protocol"
	"github.com/steelbrain/ffmpeg-over-ip/internal/rewrite"
	"github.com/steelbrain/ffmpeg-over-ip/internal/session"
)

var activeConns atomic.Int32

func isPeerAllowed(remoteAddr net.Addr, allowed []string) bool {
	if len(allowed) == 0 {
		return true // no restriction
	}
	host, _, err := net.SplitHostPort(remoteAddr.String())
	if err != nil {
		host = remoteAddr.String()
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return false
	}
	for _, cidr := range allowed {
		cidr = strings.TrimSpace(cidr)
		if cidr == "" {
			continue
		}
		// Try CIDR
		if _, ipNet, err := net.ParseCIDR(cidr); err == nil {
			if ipNet.Contains(remoteIP) {
				return true
			}
			continue
		}
		// Try exact IP
		if ip := net.ParseIP(cidr); ip != nil {
			if ip.Equal(remoteIP) {
				return true
			}
		}
	}
	return false
}

func main() {
	configPath := flag.String("config", "", "path to server config file")
	debugPaths := flag.Bool("debug-print-search-paths", false, "print config search paths and exit")
	flag.Parse()

	if *debugPaths {
		for _, p := range config.SearchPaths("server") {
			fmt.Println(p)
		}
		return
	}

	cfg, err := config.LoadServerConfig(*configPath)
	if err != nil {
		log.Fatalf("failed to load config: %v", err)
	}

	defer config.SetupLogging(cfg.Log)()

	// Resolve ffmpeg/ffprobe paths from server binary's directory
	exePath, err := os.Executable()
	if err != nil {
		log.Fatalf("failed to resolve executable path: %v", err)
	}
	exeDir := filepath.Dir(exePath)
	ffmpegPath := filepath.Join(exeDir, "ffmpeg")
	ffprobePath := filepath.Join(exeDir, "ffprobe")

	// Set up signal-aware context
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ro, rw, err := cfg.ResolveShortCircuitPaths()
	if err != nil {
		log.Fatalf("%v", err)
	}
	if len(ro) > 0 {
		log.Printf("short-circuit RO for: %v", ro)
	}
	if len(rw) > 0 {
		log.Printf("short-circuit RW for: %v", rw)
	}
	if cfg.MaxConcurrent == 0 {
		log.Printf("max concurrent: unlimited")
	} else {
		log.Printf("max concurrent: %d (default 1 for little nodes, bump for big GPU)", cfg.MaxConcurrent)
	}
	if len(cfg.AllowedPeers) > 0 {
		log.Printf("allowed peers: %v (WireGuard mesh)", cfg.AllowedPeers)
	}

	// Re-assign for closure capture (cfg has raw, but we want resolved)
	cfg.ShortCircuitRead = ro
	cfg.ShortCircuitReadWrite = rw

	network, addr := config.ParseAddress(cfg.Address)
	listener, err := net.Listen(network, addr)
	if err != nil {
		log.Fatalf("failed to listen on %s: %v", addr, err)
	}
	defer listener.Close()

	if network == "unix" {
		if err := os.Chmod(addr, 0777); err != nil {
			log.Printf("warning: failed to chmod socket: %v", err)
		}
	}

	log.Printf("listening on %s (%s)", addr, network)

	// Stop accepting on context cancellation; clean up Unix socket
	go func() {
		<-ctx.Done()
		listener.Close()
		if network == "unix" {
			os.Remove(addr)
		}
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return // shutting down
			}
			log.Printf("accept error: %v", err)
			continue
		}

		go handleConnection(ctx, conn, cfg, ffmpegPath, ffprobePath)
	}
}

func handleConnection(ctx context.Context, conn net.Conn, cfg *config.ServerConfig, ffmpegPath, ffprobePath string) {
	defer conn.Close()

	// WireGuard mesh: only allow peers in allowed list (CIDR or IP)
	if !isPeerAllowed(conn.RemoteAddr(), cfg.AllowedPeers) {
		sendError(conn, "peer not allowed")
		log.Printf("reject not allowed peer %s (allowed: %v)", conn.RemoteAddr(), cfg.AllowedPeers)
		return
	}

	// Admission control: atomic increment then check to avoid race where 2 conns both see 0 < max
	if cfg.MaxConcurrent > 0 {
		cur := activeConns.Add(1)
		if int(cur) > cfg.MaxConcurrent {
			activeConns.Add(-1)
			sendError(conn, "server busy: at capacity")
			log.Printf("reject busy from %s (%d/%d active)", conn.RemoteAddr(), cur-1, cfg.MaxConcurrent)
			return
		}
		defer activeConns.Add(-1)
	} else {
		activeConns.Add(1)
		defer activeConns.Add(-1)
	}

	// Prevent slowloris holding slot: 10s to send first message
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// Read command message
	msg, err := protocol.ReadMessageFrom(conn)
	_ = conn.SetReadDeadline(time.Time{}) // clear for session keepalive
	if err != nil {
		log.Printf("failed to read command: %v", err)
		return
	}
	if msg.Type != protocol.MsgCommand {
		sendError(conn, fmt.Sprintf("expected command message (0x%02x), got 0x%02x", protocol.MsgCommand, msg.Type))
		return
	}

	// Decode command
	cmd, err := protocol.DecodeCommandMessage(msg.Payload)
	if err != nil {
		sendError(conn, fmt.Sprintf("invalid command: %v", err))
		return
	}

	// Verify HMAC
	if !auth.Verify(cfg.AuthSecret, protocol.CurrentVersion, cmd.Nonce, cmd.Signature, cmd.Program, cmd.Args) {
		sendError(conn, "authentication failed")
		log.Printf("auth failed from %s", conn.RemoteAddr())
		return
	}

	// Determine binary path
	var binaryPath string
	switch cmd.Program {
	case protocol.ProgramFFmpeg:
		binaryPath = ffmpegPath
	case protocol.ProgramFFprobe:
		binaryPath = ffprobePath
	default:
		sendError(conn, fmt.Sprintf("unknown program: 0x%02x", cmd.Program))
		return
	}

	// Apply rewrites
	args := rewrite.Apply(cmd.Args, cfg.Rewrites)

	if cfg.Debug {
		log.Printf("[debug] original args: %v", cmd.Args)
		log.Printf("[debug] rewritten args: %v", args)
	}
	log.Printf("running %s %v (from %s)", filepath.Base(binaryPath), args, conn.RemoteAddr())

	// Start process
	proc := process.NewProcess(binaryPath, args)
	proc.SetShortCircuitPaths(cfg.ShortCircuitRead, cfg.ShortCircuitReadWrite)
	if err := proc.Start(ctx); err != nil {
		sendError(conn, fmt.Sprintf("failed to start process: %v", err))
		return
	}

	// Run session
	sess := session.NewSession(conn, proc)
	exitCode, err := sess.Run(ctx)
	if err != nil {
		log.Printf("session error: %v", err)
	}

	log.Printf("process exited with code %d (from %s)", exitCode, conn.RemoteAddr())
}

func sendError(conn net.Conn, msg string) {
	protocol.WriteMessageTo(conn, protocol.MsgError, []byte(msg))
}

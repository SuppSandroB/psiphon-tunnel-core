/*
 * Copyright (c) 2014, Psiphon Inc.
 * All rights reserved.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package psiphon

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"code.google.com/p/go.crypto/ssh"
)

// Tunneler specifies the interface required by components that use a tunnel.
// Components which use this interface may be serviced by a single Tunnel instance,
// or a Controller which manages a pool of tunnels, or any other object which
// implements Tunneler.
type Tunneler interface {
	Dial(remoteAddr string) (conn net.Conn, err error)
	SignalFailure()
}

const (
	TUNNEL_PROTOCOL_SSH            = "SSH"
	TUNNEL_PROTOCOL_OBFUSCATED_SSH = "OSSH"
	TUNNEL_PROTOCOL_UNFRONTED_MEEK = "UNFRONTED-MEEK-OSSH"
	TUNNEL_PROTOCOL_FRONTED_MEEK   = "FRONTED-MEEK-OSSH"
)

// This is a list of supported tunnel protocols, in default preference order
var SupportedTunnelProtocols = []string{
	TUNNEL_PROTOCOL_FRONTED_MEEK,
	TUNNEL_PROTOCOL_UNFRONTED_MEEK,
	TUNNEL_PROTOCOL_OBFUSCATED_SSH,
	TUNNEL_PROTOCOL_SSH,
}

// Tunnel is a connection to a Psiphon server. An established
// tunnel includes a network connection to the specified server
// and an SSH session built on top of that transport.
type Tunnel struct {
	serverEntry             *ServerEntry
	sessionId               string
	sessionStarted          int32
	protocol                string
	conn                    Conn
	sshClient               *ssh.Client
	sshKeepAliveQuit        chan struct{}
	portForwardFailures     chan int
	portForwardFailureTotal int
}

// EstablishTunnel first makes a network transport connection to the
// Psiphon server and then establishes an SSH client session on top of
// that transport. The SSH server is authenticated using the public
// key in the server entry.
// Depending on the server's capabilities, the connection may use
// plain SSH over TCP, obfuscated SSH over TCP, or obfuscated SSH over
// HTTP (meek protocol).
// When requiredProtocol is not blank, that protocol is used. Otherwise,
// the first protocol in SupportedTunnelProtocols that's also in the
// server capabilities is used.
func EstablishTunnel(
	config *Config, pendingConns *Conns, serverEntry *ServerEntry) (tunnel *Tunnel, err error) {

	// Select the protocol
	var selectedProtocol string
	// TODO: properly handle protocols (e.g. FRONTED-MEEK-OSSH) vs. capabilities (e.g., {FRONTED-MEEK, OSSH})
	// for now, the code is simply assuming that MEEK capabilities imply OSSH capability.
	if config.TunnelProtocol != "" {
		requiredCapability := strings.TrimSuffix(config.TunnelProtocol, "-OSSH")
		if !Contains(serverEntry.Capabilities, requiredCapability) {
			return nil, ContextError(fmt.Errorf("server does not have required capability"))
		}
		selectedProtocol = config.TunnelProtocol
	} else {
		// Order of SupportedTunnelProtocols is default preference order
		for _, protocol := range SupportedTunnelProtocols {
			requiredCapability := strings.TrimSuffix(protocol, "-OSSH")
			if Contains(serverEntry.Capabilities, requiredCapability) {
				selectedProtocol = protocol
				break
			}
		}
		if selectedProtocol == "" {
			return nil, ContextError(fmt.Errorf("server does not have any supported capabilities"))
		}
	}
	Notice(NOTICE_INFO, "connecting to %s in region %s using %s",
		serverEntry.IpAddress, serverEntry.Region, selectedProtocol)

	// The meek protocols tunnel obfuscated SSH. Obfuscated SSH is layered on top of SSH.
	// So depending on which protocol is used, multiple layers are initialized.
	port := 0
	useMeek := false
	useFronting := false
	useObfuscatedSsh := false
	switch selectedProtocol {
	case TUNNEL_PROTOCOL_FRONTED_MEEK:
		useMeek = true
		useFronting = true
		useObfuscatedSsh = true
	case TUNNEL_PROTOCOL_UNFRONTED_MEEK:
		useMeek = true
		useObfuscatedSsh = true
		port = serverEntry.SshObfuscatedPort
	case TUNNEL_PROTOCOL_OBFUSCATED_SSH:
		useObfuscatedSsh = true
		port = serverEntry.SshObfuscatedPort
	case TUNNEL_PROTOCOL_SSH:
		port = serverEntry.SshPort
	}

	// Generate a session Id for the Psiphon server API. This is generated now so
	// that it can be sent with the SSH password payload, which helps the server
	// associate client geo location, used in server API stats, with the session ID.
	sessionId, err := MakeSessionId()
	if err != nil {
		return nil, ContextError(err)
	}

	// Create the base transport: meek or direct connection
	dialConfig := &DialConfig{
		ConnectTimeout:             TUNNEL_CONNECT_TIMEOUT,
		ReadTimeout:                TUNNEL_READ_TIMEOUT,
		WriteTimeout:               TUNNEL_WRITE_TIMEOUT,
		PendingConns:               pendingConns,
		BindToDeviceServiceAddress: config.BindToDeviceServiceAddress,
		BindToDeviceDnsServer:      config.BindToDeviceDnsServer,
	}
	var conn Conn
	if useMeek {
		conn, err = DialMeek(serverEntry, sessionId, useFronting, dialConfig)
		if err != nil {
			return nil, ContextError(err)
		}
		// TODO: MeekConn doesn't go into pendingConns since there's no direct connection to
		// interrupt; underlying HTTP connections may be candidates for interruption, but only
		// after relay starts polling...
	} else {
		conn, err = DialTCP(fmt.Sprintf("%s:%d", serverEntry.IpAddress, port), dialConfig)
		if err != nil {
			return nil, ContextError(err)
		}
	}
	defer func() {
		// Cleanup on error
		if err != nil {
			conn.Close()
		}
	}()

	// Add obfuscated SSH layer
	var sshConn net.Conn
	sshConn = conn
	if useObfuscatedSsh {
		sshConn, err = NewObfuscatedSshConn(conn, serverEntry.SshObfuscatedKey)
		if err != nil {
			return nil, ContextError(err)
		}
	}

	// Now establish the SSH session over the sshConn transport
	expectedPublicKey, err := base64.StdEncoding.DecodeString(serverEntry.SshHostKey)
	if err != nil {
		return nil, ContextError(err)
	}
	sshCertChecker := &ssh.CertChecker{
		HostKeyFallback: func(addr string, remote net.Addr, publicKey ssh.PublicKey) error {
			if !bytes.Equal(expectedPublicKey, publicKey.Marshal()) {
				return ContextError(errors.New("unexpected host public key"))
			}
			return nil
		},
	}
	sshPasswordPayload, err := json.Marshal(
		struct {
			SessionId   string `json:"SessionId"`
			SshPassword string `json:"SshPassword"`
		}{sessionId, serverEntry.SshPassword})
	if err != nil {
		return nil, ContextError(err)
	}
	sshClientConfig := &ssh.ClientConfig{
		User: serverEntry.SshUsername,
		Auth: []ssh.AuthMethod{
			ssh.Password(string(sshPasswordPayload)),
		},
		HostKeyCallback: sshCertChecker.CheckHostKey,
	}
	// The folowing is adapted from ssh.Dial(), here using a custom conn
	// The sshAddress is passed through to host key verification callbacks; we don't use it.
	sshAddress := ""
	sshClientConn, sshChans, sshReqs, err := ssh.NewClientConn(sshConn, sshAddress, sshClientConfig)
	if err != nil {
		return nil, ContextError(err)
	}
	sshClient := ssh.NewClient(sshClientConn, sshChans, sshReqs)

	// Run a goroutine to periodically execute SSH keepalive
	sshKeepAliveQuit := make(chan struct{})
	sshKeepAliveTicker := time.NewTicker(TUNNEL_SSH_KEEP_ALIVE_PERIOD)
	go func() {
		for {
			select {
			case <-sshKeepAliveTicker.C:
				_, _, err := sshClient.SendRequest("keepalive@openssh.com", true, nil)
				if err != nil {
					Notice(NOTICE_ALERT, "ssh keep alive failed: %s", err)
					// TODO: call Tunnel.Close()?
					sshKeepAliveTicker.Stop()
					conn.Close()
				}
			case <-sshKeepAliveQuit:
				sshKeepAliveTicker.Stop()
				return
			}
		}
	}()

	return &Tunnel{
			serverEntry:      serverEntry,
			sessionId:        sessionId,
			protocol:         selectedProtocol,
			conn:             conn,
			sshClient:        sshClient,
			sshKeepAliveQuit: sshKeepAliveQuit,
			// portForwardFailures buffer size is large enough to receive the thresold number
			// of failure reports without blocking. Senders can drop failures without blocking.
			portForwardFailures: make(chan int, config.PortForwardFailureThreshold)},
		nil
}

// Close terminates the tunnel.
func (tunnel *Tunnel) Close() {
	if tunnel.sshKeepAliveQuit != nil {
		close(tunnel.sshKeepAliveQuit)
	}
	if tunnel.conn != nil {
		tunnel.conn.Close()
	}
}

func (tunnel *Tunnel) IsSessionStarted() bool {
	return atomic.LoadInt32(&tunnel.sessionStarted) == 1
}

func (tunnel *Tunnel) SetSessionStarted() {
	atomic.StoreInt32(&tunnel.sessionStarted, 1)
}

// Dial establishes a port forward connection through the tunnel
func (tunnel *Tunnel) Dial(remoteAddr string) (conn net.Conn, err error) {
	// TODO: should this track port forward failures as in Controller.DialWithTunnel?
	return tunnel.sshClient.Dial("tcp", remoteAddr)
}

// SignalFailure notifies the tunnel that an associated component has failed.
// This will terminate the tunnel.
func (tunnel *Tunnel) SignalFailure() {
	Notice(NOTICE_ALERT, "tunnel received failure signal")
	tunnel.Close()
}

// GetServerID provides a unique identifier for the server the tunnel connects to.
// This ID is consistent between multiple tunnels connected to that server.
func (tunnel *Tunnel) GetServerID() string {
	return tunnel.serverEntry.IpAddress
}

// Copyright (C) 2023  mieru authors
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

package protocolv2

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	mrand "math/rand"
	"net"
	"sync"
	"time"

	"github.com/enfein/mieru/pkg/appctl/appctlpb"
	"github.com/enfein/mieru/pkg/cipher"
	"github.com/enfein/mieru/pkg/log"
	"github.com/enfein/mieru/pkg/mathext"
	"github.com/enfein/mieru/pkg/stderror"
	"github.com/enfein/mieru/pkg/util"
	"github.com/enfein/mieru/pkg/util/sockopts"
)

const idleUnderlayTickerInterval = 5 * time.Second

// Mux manages the sessions and underlays.
type Mux struct {
	// ---- common fields ----
	isClient    bool
	endpoints   []UnderlayProperties
	underlays   []Underlay
	chAccept    chan net.Conn
	chAcceptErr chan error
	used        bool
	done        chan struct{}
	mu          sync.Mutex
	cleaner     *time.Ticker

	// ---- client fields ----
	password        []byte
	multiplexFactor int

	// ---- server fields ----
	users map[string]*appctlpb.User
}

var _ net.Listener = &Mux{}

// NewMux creates a new mieru v2 multiplex controller.
func NewMux(isClinet bool) *Mux {
	if isClinet {
		log.Infof("Initializing client multiplexer")
	} else {
		log.Infof("Initializing server multiplexer")
	}
	mux := &Mux{
		isClient:    isClinet,
		underlays:   make([]Underlay, 0),
		chAccept:    make(chan net.Conn, sessionChanCapacity),
		chAcceptErr: make(chan error, 1), // non-blocking
		done:        make(chan struct{}),
		cleaner:     time.NewTicker(idleUnderlayTickerInterval),
	}

	// Run idle underlay cleaner in the background.
	go func() {
		for {
			select {
			case <-mux.cleaner.C:
				mux.mu.Lock()
				mux.cleanUnderlay()
				mux.mu.Unlock()
			case <-mux.done:
				mux.cleaner.Stop()
				return
			}
		}
	}()
	return mux
}

func (m *Mux) SetClientPassword(password []byte) *Mux {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isClient {
		panic("Can't set client password in server mux")
	}
	if m.used {
		panic("Can't set client password after mux is used")
	}
	m.password = password
	return m
}

func (m *Mux) SetClientMultiplexFactor(n int) *Mux {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.isClient {
		panic("Can't set multiplex factor in server mux")
	}
	if m.used {
		panic("Can't set multiplex factor after mux is used")
	}
	m.multiplexFactor = mathext.Max(n, 0)
	log.Infof("Mux multiplexing factor is set to %d", m.multiplexFactor)
	return m
}

func (m *Mux) SetServerUsers(users map[string]*appctlpb.User) *Mux {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.isClient {
		panic("Can't set server users in client mux")
	}
	if m.used {
		panic("Can't set server users after mux is used")
	}
	m.users = users
	return m
}

func (m *Mux) SetEndpoints(endpoints []UnderlayProperties) *Mux {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.used {
		panic("Can't set endpoints after mux is used")
	}
	m.endpoints = endpoints
	return m
}

func (m *Mux) Accept() (net.Conn, error) {
	select {
	case err := <-m.chAcceptErr:
		return nil, err
	case conn := <-m.chAccept:
		return conn, nil
	case <-m.done:
		return nil, io.EOF
	}
}

func (m *Mux) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	select {
	case <-m.done:
		return nil
	default:
	}

	if m.isClient {
		log.Infof("Closing client multiplexer")
	} else {
		log.Infof("Closing server multiplexer")
	}
	for _, underlay := range m.underlays {
		underlay.Close()
	}
	m.underlays = make([]Underlay, 0)
	close(m.done)
	return nil
}

// Addr is not supported by Mux.
func (m *Mux) Addr() net.Addr {
	return util.NilNetAddr()
}

// Start listens on all the server addresses for incoming connections.
// Call this method in client results in an error.
// This method doesn't block.
func (m *Mux) Start() error {
	if m.isClient {
		return stderror.ErrInvalidOperation
	}
	if len(m.users) == 0 {
		return fmt.Errorf("no user found")
	}
	if len(m.endpoints) == 0 {
		return fmt.Errorf("no server listening endpoint found")
	}
	for _, p := range m.endpoints {
		if util.IsNilNetAddr(p.LocalAddr()) {
			return fmt.Errorf("endpoint local address is not set")
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.used = true
	for _, p := range m.endpoints {
		go m.acceptUnderlayLoop(p)
	}
	return nil
}

// DialContext returns a network connection for the client to consume.
// The connection may be a session established from an existing underlay.
func (m *Mux) DialContext(ctx context.Context) (net.Conn, error) {
	if !m.isClient {
		return nil, stderror.ErrInvalidOperation
	}
	if len(m.password) == 0 {
		return nil, fmt.Errorf("client password is not set")
	}
	if len(m.endpoints) == 0 {
		return nil, fmt.Errorf("no server listening endpoint found")
	}
	for _, p := range m.endpoints {
		if util.IsNilNetAddr(p.RemoteAddr()) {
			return nil, fmt.Errorf("endpoint remote address is not set")
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.used = true
	var err error

	// Try to find a underlay for the session.
	m.cleanUnderlay()
	underlay := m.maybePickExistingUnderlay()
	if underlay == nil {
		underlay, err = m.newUnderlay(ctx)
		if err != nil {
			return nil, err
		}
		log.Debugf("Created new underlay %v", underlay)
	} else {
		log.Debugf("Reusing existing underlay %v", underlay)
	}

	if ok := underlay.Scheduler().IncPending(); !ok {
		// This underlay can't be used. Create a new one.
		underlay, err = m.newUnderlay(ctx)
		if err != nil {
			return nil, err
		}
		log.Debugf("Created yet another new underlay %v", underlay)
		underlay.Scheduler().IncPending()
	}
	defer func() {
		underlay.Scheduler().DecPending()
	}()
	session := NewSession(mrand.Uint32(), true, underlay.MTU())
	if err := underlay.AddSession(session, nil); err != nil {
		return nil, fmt.Errorf("AddSession() failed: %v", err)
	}
	return session, nil
}

func (m *Mux) acceptUnderlayLoop(properties UnderlayProperties) {
	laddr := properties.LocalAddr().String()
	if laddr == "" {
		m.chAcceptErr <- fmt.Errorf("underlay local address is empty")
		return
	}

	network := properties.LocalAddr().Network()
	switch network {
	case "tcp", "tcp4", "tcp6":
		listenConfig := net.ListenConfig{
			Control: sockopts.ReuseAddrPort(),
		}
		rawListener, err := listenConfig.Listen(context.Background(), network, laddr)
		if err != nil {
			m.chAcceptErr <- fmt.Errorf("Listen() failed: %w", err)
			return
		}
		log.Infof("Mux is listening to endpoint %s %s", network, laddr)
		for {
			underlay, err := m.acceptTCPUnderlay(rawListener, properties)
			if err != nil {
				m.chAcceptErr <- err
				return
			}
			log.Debugf("Created new server underlay %v", underlay)
			m.mu.Lock()
			m.underlays = append(m.underlays, underlay)
			m.cleanUnderlay()
			m.mu.Unlock()
			UnderlayPassiveOpens.Add(1)
			currEst := UnderlayCurrEstablished.Add(1)
			maxConn := UnderlayMaxConn.Load()
			if currEst > maxConn {
				UnderlayMaxConn.Store(currEst)
			}

			go func() {
				err := underlay.RunEventLoop(context.Background())
				if err != nil && !stderror.IsEOF(err) && !stderror.IsClosed(err) {
					log.Debugf("%v RunEventLoop(): %v", underlay, err)
				}
				underlay.Close()
			}()

			go func() {
				for {
					conn, err := underlay.Accept()
					if err != nil {
						if !stderror.IsEOF(err) && !stderror.IsClosed(err) {
							log.Debugf("%v Accept(): %v", underlay, err)
						}
						break
					}
					m.chAccept <- conn
				}
			}()
		}
	case "udp", "udp4", "udp6":
		conn, err := net.ListenUDP(network, properties.LocalAddr().(*net.UDPAddr))
		if err != nil {
			m.chAcceptErr <- fmt.Errorf("ListenUDP() failed: %w", err)
			return
		}
		rawConn, err := conn.SyscallConn()
		if err != nil {
			m.chAcceptErr <- fmt.Errorf("SyscallConn() failed: %w", err)
			return
		}
		rawConn.Control(sockopts.ReuseAddrPortRaw())
		log.Infof("Mux is listening to endpoint %s %s", network, laddr)
		underlay := &UDPUnderlay{
			baseUnderlay:      *newBaseUnderlay(false, properties.MTU()),
			conn:              conn,
			idleSessionTicker: time.NewTicker(idleSessionTickerInterval),
			users:             m.users,
		}
		log.Infof("Created new server underlay %v", underlay)
		m.mu.Lock()
		m.underlays = append(m.underlays, underlay)
		m.cleanUnderlay()
		m.mu.Unlock()
		UnderlayPassiveOpens.Add(1)
		currEst := UnderlayCurrEstablished.Add(1)
		maxConn := UnderlayMaxConn.Load()
		if currEst > maxConn {
			UnderlayMaxConn.Store(currEst)
		}

		go func() {
			err := underlay.RunEventLoop(context.Background())
			if err != nil && !stderror.IsEOF(err) && !stderror.IsClosed(err) {
				log.Debugf("%v RunEventLoop(): %v", underlay, err)
			}
			underlay.Close()
		}()

		go func() {
			for {
				conn, err := underlay.Accept()
				if err != nil {
					if !stderror.IsEOF(err) && !stderror.IsClosed(err) {
						log.Debugf("%v Accept(): %v", underlay, err)
					}
					break
				}
				m.chAccept <- conn
			}
		}()
	default:
		m.chAcceptErr <- fmt.Errorf("unsupported underlay network type %q", network)
	}
}

func (m *Mux) acceptTCPUnderlay(rawListener net.Listener, properties UnderlayProperties) (Underlay, error) {
	rawConn, err := rawListener.Accept()
	if err != nil {
		return nil, fmt.Errorf("Accept() underlay failed: %w", err)
	}
	return m.serverWrapTCPConn(rawConn, properties.MTU(), m.users), nil
}

func (m *Mux) serverWrapTCPConn(rawConn net.Conn, mtu int, users map[string]*appctlpb.User) Underlay {
	var err error
	var blocks []cipher.BlockCipher
	for _, user := range users {
		var password []byte
		password, err = hex.DecodeString(user.GetHashedPassword())
		if err != nil {
			log.Debugf("Unable to decode hashed password %q from user %q", user.GetHashedPassword(), user.GetName())
			continue
		}
		if len(password) == 0 {
			password = cipher.HashPassword([]byte(user.GetPassword()), []byte(user.GetName()))
		}
		blocksFromUser, err := cipher.BlockCipherListFromPassword(password, false)
		if err != nil {
			log.Debugf("Unable to create block cipher of user %q", user.GetName())
			continue
		}
		for _, block := range blocksFromUser {
			block.SetBlockContext(cipher.BlockContext{
				UserName: user.GetName(),
			})
		}
		blocks = append(blocks, blocksFromUser...)
	}
	return &TCPUnderlay{
		baseUnderlay: *newBaseUnderlay(false, mtu),
		conn:         rawConn.(*net.TCPConn),
		candidates:   blocks,
		users:        users,
	}
}

// newUnderlay returns a new underlay.
// This method MUST be called only when holding the mu lock.
func (m *Mux) newUnderlay(ctx context.Context) (Underlay, error) {
	var underlay Underlay
	i := mrand.Intn(len(m.endpoints))
	p := m.endpoints[i]
	switch p.TransportProtocol() {
	case util.TCPTransport:
		block, err := cipher.BlockCipherFromPassword(m.password, false)
		if err != nil {
			return nil, fmt.Errorf("cipher.BlockCipherFromPassword() failed: %v", err)
		}
		underlay, err = NewTCPUnderlay(ctx, p.RemoteAddr().Network(), "", p.RemoteAddr().String(), p.MTU(), block)
		if err != nil {
			return nil, fmt.Errorf("NewTCPUnderlay() failed: %v", err)
		}
	case util.UDPTransport:
		block, err := cipher.BlockCipherFromPassword(m.password, true)
		if err != nil {
			return nil, fmt.Errorf("cipher.BlockCipherFromPassword() failed: %v", err)
		}
		underlay, err = NewUDPUnderlay(ctx, p.RemoteAddr().Network(), "", p.RemoteAddr().String(), p.MTU(), block)
		if err != nil {
			return nil, fmt.Errorf("NewUDPUnderlay() failed: %v", err)
		}
	default:
		return nil, fmt.Errorf("unsupport transport protocol %v", p.TransportProtocol())
	}
	m.underlays = append(m.underlays, underlay)
	UnderlayActiveOpens.Add(1)
	currEst := UnderlayCurrEstablished.Add(1)
	maxConn := UnderlayMaxConn.Load()
	if currEst > maxConn {
		UnderlayMaxConn.Store(currEst)
	}
	go func() {
		err := underlay.RunEventLoop(ctx)
		if err != nil && !stderror.IsEOF(err) && !stderror.IsClosed(err) {
			log.Debugf("%v RunEventLoop(): %v", underlay, err)
		}
		underlay.Close()
	}()
	return underlay, nil
}

// maybePickExistingUnderlay returns either an existing underlay that
// can be used by a session, or nil. In the later case a new underlay
// should be created.
// This method MUST be called only when holding the mu lock.
func (m *Mux) maybePickExistingUnderlay() Underlay {
	active := make([]Underlay, 0)
	for _, underlay := range m.underlays {
		select {
		case <-underlay.Done():
		default:
			if !underlay.Scheduler().IsDisabled() {
				active = append(active, underlay)
			}
		}
	}

	if m.multiplexFactor > 0 {
		reuseUnderlayFactor := len(active) * m.multiplexFactor
		n := mrand.Intn(reuseUnderlayFactor + 1)
		if n < reuseUnderlayFactor {
			return active[n/m.multiplexFactor]
		}
	}
	return nil
}

// cleanUnderlay removes closed underlays.
// This method MUST be called only when holding the mu lock.
func (m *Mux) cleanUnderlay() {
	remaining := make([]Underlay, 0)
	cnt := 0
	for _, underlay := range m.underlays {
		select {
		case <-underlay.Done():
		default:
			if underlay.Scheduler().Idle() {
				underlay.Close()
				cnt++
			} else {
				remaining = append(remaining, underlay)
			}
		}
	}
	m.underlays = remaining
	if cnt > 0 {
		log.Debugf("Mux cleaned %d underlays", cnt)
	}
}

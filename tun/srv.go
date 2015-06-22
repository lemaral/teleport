package tun

import (
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gravitational/teleport/auth"
	"github.com/gravitational/teleport/backend"
	"github.com/gravitational/teleport/sshutils"
	"github.com/gravitational/teleport/utils"

	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/gravitational/roundtrip"
	"github.com/gravitational/teleport/Godeps/_workspace/src/github.com/mailgun/log"
	"github.com/gravitational/teleport/Godeps/_workspace/src/golang.org/x/crypto/ssh"
	"github.com/mailgun/oxy/forward"
)

type RemoteSite interface {
	ConnectToServer(addr, user string, auth []ssh.AuthMethod) (*ssh.Client, error)
	GetLastConnected() time.Time
	GetName() string
	GetServers() ([]backend.Server, error)
	GetStatus() string
	GetEvents() ([]interface{}, error)
}

type Server interface {
	GetSites() []RemoteSite
	GetSite(name string) (RemoteSite, error)
	Start() error
	Wait()
}

type server struct {
	sync.RWMutex

	certChecker ssh.CertChecker
	l           net.Listener
	srv         *sshutils.Server

	sites []*remoteSite
}

// New returns an unstarted server
func NewServer(addr utils.NetAddr, hostSigners []ssh.Signer) (Server, error) {
	srv := &server{
		sites: []*remoteSite{},
	}
	s, err := sshutils.NewServer(
		addr,
		srv,
		hostSigners,
		sshutils.AuthMethods{
			PublicKey: srv.keyAuth,
		})
	if err != nil {
		return nil, err
	}
	srv.certChecker = ssh.CertChecker{IsAuthority: srv.isAuthority}
	srv.srv = s
	return srv, nil
}

func (s *server) Wait() {
	s.srv.Wait()
}

func (s *server) Addr() string {
	return s.srv.Addr()
}

func (s *server) Start() error {
	return s.srv.Start()
}

func (s *server) Close() error {
	return s.srv.Close()
}

func (s *server) HandleNewChan(sconn *ssh.ServerConn, nch ssh.NewChannel) {
	log.Infof("got new channel request: %v", nch.ChannelType())
	switch nch.ChannelType() {
	case chanHeartbeat:
		log.Infof("got heartbeat request from agent: %v", sconn)
		site, err := s.upsertSite(sconn)
		if err != nil {
			log.Errorf("failed to upsert site: %v", err)
			nch.Reject(ssh.ConnectionFailed, "failed to upsert site")
			return
		}
		ch, req, err := nch.Accept()
		if err != nil {
			log.Errorf("failed to accept channel: %v", err)
			sconn.Close()
			return
		}
		go site.handleHeartbeat(ch, req)
	}
}

// isAuthority is called during checking the client key, to see if the signing
// key is the real CA authority key.
func (s *server) isAuthority(auth ssh.PublicKey) bool {
	return true
}

func (s *server) keyAuth(
	conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
	cid := fmt.Sprintf(
		"conn(%v->%v, user=%v)", conn.RemoteAddr(),
		conn.LocalAddr(), conn.User())

	log.Infof("%v auth attempt with key %v", cid, key.Type())
	return nil, nil
}

func (s *server) upsertSite(c ssh.Conn) (*remoteSite, error) {
	s.Lock()
	defer s.Unlock()

	fqdn := c.User()
	var site *remoteSite
	for _, st := range s.sites {
		if st.fqdn == fqdn {
			site = st
			break
		}
	}
	if site != nil {
		if err := site.init(c); err != nil {
			return nil, err
		}
	} else {
		site = &remoteSite{srv: s, fqdn: c.User()}
		if err := site.init(c); err != nil {
			return nil, err
		}
		s.sites = append(s.sites, site)
	}
	return site, nil
}

func (s *server) GetSites() []RemoteSite {
	s.RLock()
	defer s.RUnlock()
	out := make([]RemoteSite, len(s.sites))
	for i := range s.sites {
		out[i] = s.sites[i]
	}
	return out
}

func (s *server) GetSite(fqdn string) (RemoteSite, error) {
	s.RLock()
	defer s.RUnlock()
	for i := range s.sites {
		if s.sites[i].fqdn == fqdn {
			return s.sites[i], nil
		}
	}
	return nil, fmt.Errorf("site not found")
}

type remoteSite struct {
	fqdn       string `json:"fqdn"`
	conn       ssh.Conn
	lastActive time.Time
	srv        *server
	clt        *auth.Client
}

func (s *remoteSite) GetEvents() ([]interface{}, error) {
	return s.clt.GetEvents()
}

func (s *remoteSite) String() string {
	return fmt.Sprintf("remoteSite(%v)", s.fqdn)
}

func (s *remoteSite) init(c ssh.Conn) error {
	if s.conn != nil {
		log.Infof("%v found site, closing previous connection", s)
		s.conn.Close()
	}
	s.conn = c
	tr := &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			ch, _, err := s.conn.OpenChannel(chanAccessPoint, nil)
			if err != nil {
				log.Errorf("remoteSite:authProxy %v", err)
				return nil, err
			}
			return newChConn(s.conn, ch), nil
		},
	}
	clt, err := auth.NewClient(
		"http://stub:0",
		roundtrip.HTTPClient(&http.Client{
			Transport: tr,
		}))
	if err != nil {
		return err
	}
	s.clt = clt
	return nil
}

func (s *remoteSite) GetStatus() string {
	diff := time.Now().Sub(s.lastActive)
	if diff > 2*heartbeatPeriod {
		return RemoteSiteStatusOffline
	}
	return RemoteSiteStatusOnline
}

func (s *remoteSite) handleHeartbeat(ch ssh.Channel, reqC <-chan *ssh.Request) {
	go func() {
		for {
			req := <-reqC
			if req == nil {
				log.Infof("agent disconnected")
				return
			}
			log.Infof("%v -> ping", s)
			s.lastActive = time.Now()
		}
	}()
}

func (s *remoteSite) GetName() string {
	return s.fqdn
}

func (s *remoteSite) GetLastConnected() time.Time {
	return s.lastActive
}

func (s *remoteSite) ConnectToServer(server, user string, auth []ssh.AuthMethod) (*ssh.Client, error) {
	ch, _, err := s.conn.OpenChannel(chanTransport, nil)
	if err != nil {
		log.Errorf("remoteSite:connectToServer %v", err)
		return nil, err
	}
	// ask remote channel to dial
	dialed, err := ch.SendRequest(chanTransportDialReq, true, []byte(server))
	if err != nil {
		log.Errorf("failed to process request: %v", err)
		return nil, err
	}
	if !dialed {
		log.Errorf("remote end failed to dial: %v", err)
		return nil, fmt.Errorf("remote server %v is not available", server)
	}
	transportConn := newChConn(s.conn, ch)
	conn, chans, reqs, err := ssh.NewClientConn(
		transportConn, server,
		&ssh.ClientConfig{
			User: user,
			Auth: auth,
		})
	if err != nil {
		log.Errorf("remoteSite:connectToServer %v", err)
		return nil, err
	}
	return ssh.NewClient(conn, chans, reqs), nil
}

func (s *remoteSite) GetServers() ([]backend.Server, error) {
	return s.clt.GetServers()
}

func (s *remoteSite) handleAuthProxy(w http.ResponseWriter, r *http.Request) {
	tr := &http.Transport{
		Dial: func(network, addr string) (net.Conn, error) {
			ch, _, err := s.conn.OpenChannel(chanAccessPoint, nil)
			if err != nil {
				log.Errorf("remoteSite:authProxy %v", err)
				return nil, err
			}
			return newChConn(s.conn, ch), nil
		},
	}

	fwd, err := forward.New(forward.RoundTripper(tr), forward.Logger(log.GetLogger()))
	if err != nil {
		log.Errorf("write: %v", err)
		roundtrip.ReplyJSON(w, http.StatusInternalServerError, err.Error())
		return
	}
	r.URL.Scheme = "http"
	r.URL.Host = "stub"
	fwd.ServeHTTP(w, r)
}

func newChConn(conn ssh.Conn, ch ssh.Channel) *chConn {
	c := &chConn{}
	c.Channel = ch
	c.conn = conn
	return c
}

type chConn struct {
	ssh.Channel
	conn ssh.Conn
}

func (c *chConn) LocalAddr() net.Addr {
	return c.conn.LocalAddr()
}

func (c *chConn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

func (c *chConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *chConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *chConn) SetWriteDeadline(t time.Time) error {
	return nil
}

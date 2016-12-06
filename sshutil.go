package sshego

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net"
	"strings"
	"time"

	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"
	"golang.org/x/crypto/ssh"
)

type kiCliHelp struct {
	passphrase string
	toptUrl    string
}

// helper assists ssh client with keyboard-interactive
// password and TOPT login. Must match the
// prototype KeyboardInteractiveChallenge.
func (ki *kiCliHelp) helper(user string, instruction string, questions []string, echos []bool) ([]string, error) {
	var answers []string
	for _, q := range questions {
		switch q {
		case passwordChallenge: // "password: "
			answers = append(answers, ki.passphrase)
		case gauthChallenge: // "google-authenticator-code: "
			w, err := otp.NewKeyFromURL(ki.toptUrl)
			panicOn(err)
			code, err := totp.GenerateCode(w.Secret(), time.Now())
			panicOn(err)
			answers = append(answers, code)
		default:
			panic(fmt.Sprintf("unrecognized challenge: '%v'", q))
		}
	}
	return answers, nil
}

func defaultFileFormat() string {
	// either ".gob.snappy" or ".json.snappy"
	return ".json.snappy"
	//return ".gob.snappy"
}

// HostState recognizes host keys are legitimate or
// impersonated, new, banned, or consitent with
// what we've seen before and so OK.
type HostState int

// Unknown means we don't have a matching stored host key.
const Unknown HostState = 0

// Banned means the host has been marked as forbidden.
const Banned HostState = 1

// KnownOK means the host key matches one we have
// previously allowed.
const KnownOK HostState = 2

// KnownRecordMismatch means we have a records
// for this IP/host-key, but either the IP or
// the host-key has varied and so it could
// be a Man-in-the-middle attack.
const KnownRecordMismatch HostState = 3

// AddedNew means the -new flag was given
// and we allowed the addition of a new
// host-key for the first time.
const AddedNew HostState = 4

func (s HostState) String() string {
	switch s {
	case Unknown:
		return "Unknown"
	case Banned:
		return "Banned"
	case KnownOK:
		return "KnownOK"
	case KnownRecordMismatch:
		return "KnownRecordMismatch"
	case AddedNew:
		return "AddedNew"
	}
	return ""
}

// HostAlreadyKnown checks the given host details against our
// known hosts file.
func (h *KnownHosts) HostAlreadyKnown(hostname string, remote net.Addr, key ssh.PublicKey, pubBytes []byte, addIfNotKnown bool, allowOneshotConnect bool) (HostState, *ServerPubKey, error) {
	strPubBytes := string(pubBytes)

	p("in HostAlreadyKnown... starting.")

	record, ok := h.Hosts[strPubBytes]
	if ok {
		if record.ServerBanned {
			err := fmt.Errorf("the key '%s' has been marked as banned", strPubBytes)
			p("in HostAlreadyKnown, returning Banned: '%s'", err)
			return Banned, record, err
		}

		if strings.HasPrefix(hostname, "localhost") || strings.HasPrefix(hostname, "127.0.0.1") {
			// no host checking when coming from localhost
			p("in HostAlreadyKnown, no host checking when coming from localhost, returning KnownOK")
			if addIfNotKnown {
				msg := fmt.Errorf("error: flag -new given but not needed; re-run without -new")
				p(msg.Error())
				return KnownOK, record, msg
			}
			return KnownOK, record, nil
		}
		if record.Hostname != hostname {
			err := fmt.Errorf("hostname mismatch for key '%s': record.Hostname:'%v' in records, hostname:'%s' supplied now", strPubBytes, record.Hostname, hostname)
			//fmt.Printf("\n in HostAlreadyKnown, returning KnownRecordMismatch: '%s'", err)
			return KnownRecordMismatch, record, err
		}
		p("in HostAlreadyKnown, returning KnownOK.")
		if addIfNotKnown {
			msg := fmt.Errorf("error: flag -new given but not needed; re-run without -new")
			p(msg.Error())
			return KnownOK, record, msg
		}
		return KnownOK, record, nil
	}

	if addIfNotKnown {
		record = &ServerPubKey{
			Hostname: hostname,
			remote:   remote,
			//Key:      key,
			HumanKey: strPubBytes,
		}

		h.Hosts[strPubBytes] = record
		h.Sync()
		if allowOneshotConnect {
			return KnownOK, record, nil
		}
		msg := fmt.Errorf("good: add previously unknown sshd host '%v' with the -new flag. Re-run without -new now", remote)
		return AddedNew, record, msg
	}

	p("at end of HostAlreadyKnown, returning Unknown.")
	return Unknown, record, nil
}

// SSHConnect is the main entry point for the gosshtun library,
// establishing an ssh tunnel between two hosts.
//
// passphrase and toptUrl (one-time password used in challenge/response)
// are optional, but will be offered to the server if set.
//
func (cfg *SshegoConfig) SSHConnect(h *KnownHosts, username string, keypath string, sshdHost string, sshdPort uint64, passphrase string, toptUrl string) error {

	p("SSHConnect sees sshdHost:port = %s:%v", sshdHost, sshdPort)

	// the callback just after key-exchange to validate server is here
	hostKeyCallback := func(hostname string, remote net.Addr, key ssh.PublicKey) error {

		pubBytes := ssh.MarshalAuthorizedKey(key)

		hostStatus, spubkey, err := h.HostAlreadyKnown(hostname, remote, key, pubBytes, cfg.AddIfNotKnown, cfg.allowOneshotConnect)
		p("in hostKeyCallback(), hostStatus: '%s', hostname='%s', remote='%s', key.Type='%s'  key.Marshal='%s'\n", hostStatus, hostname, remote, key.Type(), pubBytes)

		h.curStatus = hostStatus
		h.curHost = spubkey

		if err != nil {
			// this is strict checking of hosts here, any non-nil error
			// will fail the ssh handshake.
			return err
		}

		switch hostStatus {
		case Banned:
			return fmt.Errorf("banned server")

		case KnownRecordMismatch:
			return fmt.Errorf("known record mismatch")

		case KnownOK:
			p("in hostKeyCallback(), hostStatus is KnownOK.")
			return nil

		case Unknown:
			// do we allow?
			return fmt.Errorf("unknown server; could be Man-In-The-Middle attack.  If this is first time setup, you must use -new to allow the new host")
		}

		return nil
	}
	// end hostKeyCallback closure definition. Has to be a closure to access h.

	// EMBEDDED SSHD server
	if cfg.EmbeddedSSHd.Addr != "" {
		log.Printf("starting -esshd with addr: %s", cfg.EmbeddedSSHd.Addr)
		err := cfg.EmbeddedSSHd.ParseAddr()
		if err != nil {
			panic(err)
		}
		cfg.NewEsshd()
		go cfg.Esshd.Start()
	}

	if cfg.RemoteToLocal.Listen.Addr != "" || cfg.LocalToRemote.Listen.Addr != "" {
		useRSA := true
		var privkey ssh.Signer
		var err error
		// to test that we fail without rsa key,
		// allow submitting auth without it
		// if the keypath == ""
		if keypath == "" {
			useRSA = false
		} else {
			// client forward tunnel with this RSA key
			privkey, err = LoadRSAPrivateKey(keypath)
			if err != nil {
				panic(err)
			}
		}

		auth := []ssh.AuthMethod{}
		if useRSA {
			auth = append(auth, ssh.PublicKeys(privkey))
		}
		if passphrase != "" {
			auth = append(auth, ssh.Password(passphrase))
		}
		if toptUrl != "" {
			ans := kiCliHelp{
				passphrase: passphrase,
				toptUrl:    toptUrl,
			}
			auth = append(auth, ssh.KeyboardInteractiveChallenge(ans.helper))
		}

		cliCfg := &ssh.ClientConfig{
			User: username,
			Auth: auth,
			// HostKeyCallback, if not nil, is called during the cryptographic
			// handshake to validate the server's host key. A nil HostKeyCallback
			// implies that all host keys are accepted.
			HostKeyCallback: hostKeyCallback,
		}
		hostport := fmt.Sprintf("%s:%d", sshdHost, sshdPort)
		p("about to ssh.Dial hostport='%s'", hostport)
		sshClientConn, err := ssh.Dial("tcp", hostport, cliCfg)
		if err != nil {
			return fmt.Errorf("sshConnect() failed at dial to '%s': '%s' ", hostport, err.Error())
		}

		if cfg.RemoteToLocal.Listen.Addr != "" {
			err = cfg.StartupReverseListener(sshClientConn)
			if err != nil {
				return fmt.Errorf("StartupReverseListener failed: %s", err)
			}
		}
		if cfg.LocalToRemote.Listen.Addr != "" {
			err = cfg.StartupForwardListener(sshClientConn)
			if err != nil {
				return fmt.Errorf("StartupFowardListener failed: %s", err)
			}
		}
	}
	return nil
}

// StartupForwardListener is called when a forward tunnel is the
// be listened for.
func (cfg *SshegoConfig) StartupForwardListener(sshClientConn *ssh.Client) error {

	p("sshego: about to listen on %s\n", cfg.LocalToRemote.Listen.Addr)
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP(cfg.LocalToRemote.Listen.Host), Port: int(cfg.LocalToRemote.Listen.Port)})
	if err != nil {
		return fmt.Errorf("could not -listen on %s: %s", cfg.LocalToRemote.Listen.Addr, err)
	}

	go func() {
		for {
			p("sshego: about to accept on local port %s\n", cfg.LocalToRemote.Listen.Addr)
			timeoutMillisec := 10000
			err = ln.SetDeadline(time.Now().Add(time.Duration(timeoutMillisec) * time.Millisecond))
			panicOn(err) // todo handle error
			fromBrowser, err := ln.Accept()
			if err != nil {
				if _, ok := err.(*net.OpError); ok {
					continue
					//break
				}
				p("ln.Accept err = '%s'  aka '%#v'\n", err, err)
				panic(err) // todo handle error
			}
			if !cfg.Quiet {
				log.Printf("sshego: accepted forward connection on %s, forwarding --> to sshd host %s, and thence --> to remote %s\n", cfg.LocalToRemote.Listen.Addr, cfg.SSHdServer.Addr, cfg.LocalToRemote.Remote.Addr)
			}

			// if you want to collect them...
			//cfg.Fwd = append(cfg.Fwd, NewForward(cfg, sshClientConn, fromBrowser))
			// or just fire and forget...
			NewForward(cfg, sshClientConn, fromBrowser)
		}
	}()

	//fmt.Printf("\n returning from SSHConnect().\n")
	return nil
}

// Fingerprint performs a SHA256 BASE64 fingerprint of the PublicKey, similar to OpenSSH.
// See: https://anongit.mindrot.org/openssh.git/commit/?id=56d1c83cdd1ac
func Fingerprint(k ssh.PublicKey) string {
	hash := sha256.Sum256(k.Marshal())
	r := "SHA256:" + base64.StdEncoding.EncodeToString(hash[:])
	return r
}

// Forwarder represents one bi-directional forward (sshego to sshd) tcp connection.
type Forwarder struct {
	shovelPair *shovelPair
}

// NewForward is called to produce a Forwarder structure for each new forward connection.
func NewForward(cfg *SshegoConfig, sshClientConn *ssh.Client, fromBrowser net.Conn) *Forwarder {

	sp := newShovelPair(false)
	channelToSSHd, err := sshClientConn.Dial("tcp", cfg.LocalToRemote.Remote.Addr)
	if err != nil {
		msg := fmt.Errorf("Remote dial to '%s' error: %s", cfg.LocalToRemote.Remote.Addr, err)
		log.Printf(msg.Error())
		return nil
	}

	// here is the heart of the ssh-secured tunnel functionality:
	// we start the two shovels that keep traffic flowing
	// in both directions from browser over to sshd:
	// reads on fromBrowser are forwarded to channelToSSHd;
	// reads on channelToSSHd are forwarded to fromBrowser.

	//sp.DoLog = true
	sp.Start(fromBrowser, channelToSSHd, "fromBrowser<-channelToSSHd", "channelToSSHd<-fromBrowser")
	return &Forwarder{shovelPair: sp}
}

// Reverse represents one bi-directional (initiated at sshd, tunneled to sshego) tcp connection.
type Reverse struct {
	shovelPair *shovelPair
}

// StartupReverseListener is called when a reverse tunnel is requested, to listen
// and tunnel those connections.
func (cfg *SshegoConfig) StartupReverseListener(sshClientConn *ssh.Client) error {
	p("StartupReverseListener called")

	addr, err := net.ResolveTCPAddr("tcp", cfg.RemoteToLocal.Listen.Addr)
	if err != nil {
		return err
	}

	lsn, err := sshClientConn.ListenTCP(addr)
	if err != nil {
		return err
	}

	// service "forwarded-tcpip" requests
	go func() {
		for {
			p("sshego: about to accept for remote addr %s\n", cfg.RemoteToLocal.Listen.Addr)
			fromRemote, err := lsn.Accept()
			if err != nil {
				if _, ok := err.(*net.OpError); ok {
					continue
					//break
				}
				p("rev.Lsn.Accept err = '%s'  aka '%#v'\n", err, err)
				panic(err) // todo handle error
			}
			if !cfg.Quiet {
				log.Printf("sshego: accepted reverse connection from remote on  %s, forwarding to --> to %s\n",
					cfg.RemoteToLocal.Listen.Addr, cfg.RemoteToLocal.Remote.Addr)
			}
			_, err = cfg.StartNewReverse(sshClientConn, fromRemote)
			if err != nil {
				log.Printf("error: StartNewReverse got error '%s'", err)
			}
		}
	}()
	return nil
}

// StartNewReverse is invoked once per reverse connection made to generate
// a new Reverse structure.
func (cfg *SshegoConfig) StartNewReverse(sshClientConn *ssh.Client, fromRemote net.Conn) (*Reverse, error) {

	channelToLocalFwd, err := net.Dial("tcp", cfg.RemoteToLocal.Remote.Addr)
	if err != nil {
		msg := fmt.Errorf("Remote dial to '%s' error: %s", cfg.RemoteToLocal.Remote.Addr, err)
		log.Printf(msg.Error())
		return nil, msg
	}

	sp := newShovelPair(false)
	rev := &Reverse{shovelPair: sp}
	sp.Start(fromRemote, channelToLocalFwd, "fromRemoter<-channelToLocalFwd", "channelToLocalFwd<-fromRemote")
	return rev, nil
}
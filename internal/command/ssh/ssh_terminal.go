package ssh

// TODO - This file was copy and pasted from the cmd package and still needs to be
// updated to take on new conventions.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/pem"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"time"

	"github.com/briandowns/spinner"
	"github.com/docker/docker/pkg/ioutils"
	"github.com/pkg/errors"
	sshCrypt "golang.org/x/crypto/ssh"

	"github.com/superfly/flyctl/agent"
	"github.com/superfly/flyctl/api"
	"github.com/superfly/flyctl/client"
	"github.com/superfly/flyctl/helpers"
	"github.com/superfly/flyctl/ssh"
	"github.com/superfly/flyctl/terminal"
)

func spin(in, out string) context.CancelFunc {
	ctx, cancel := context.WithCancel(context.Background())

	if !helpers.IsTerminal() {
		fmt.Fprintln(os.Stderr, in)
		return cancel
	}

	go func() {
		s := spinner.New(spinner.CharSets[11], 100*time.Millisecond)
		s.Writer = os.Stderr
		s.Prefix = in
		s.FinalMSG = out
		s.Start()
		defer s.Stop()

		<-ctx.Done()
	}()

	return cancel
}

type SSHParams struct {
	Ctx            context.Context
	Org            api.OrganizationImpl
	App            string
	Dialer         agent.Dialer
	Cmd            string
	Env            map[string]string
	Stdin          io.Reader
	Stdout         io.WriteCloser
	Stderr         io.WriteCloser
	DisableSpinner bool
}

func RunSSHCommand(ctx context.Context, app *api.AppCompact, dialer agent.Dialer, addr string, cmd string) ([]byte, error) {
	var inBuf bytes.Buffer
	var errBuf bytes.Buffer
	var outBuf bytes.Buffer
	stdoutWriter := ioutils.NewWriteCloserWrapper(&outBuf, func() error { return nil })
	stderrWriter := ioutils.NewWriteCloserWrapper(&errBuf, func() error { return nil })
	inReader := ioutils.NewReadCloserWrapper(&inBuf, func() error { return nil })

	err := SSHConnect(&SSHParams{
		Ctx:            ctx,
		Org:            app.Organization,
		Dialer:         dialer,
		App:            app.Name,
		Cmd:            cmd,
		Stdin:          inReader,
		Stdout:         stdoutWriter,
		Stderr:         stderrWriter,
		DisableSpinner: true,
	}, addr)
	if err != nil {
		return nil, err
	}

	if len(errBuf.Bytes()) > 0 {
		return nil, fmt.Errorf(errBuf.String())
	}

	return outBuf.Bytes(), nil
}

func SSHConnect(p *SSHParams, addr string) error {
	terminal.Debugf("Fetching certificate for %s\n", addr)

	cert, pk, err := singleUseSSHCertificate(p.Ctx, p.Org)
	if err != nil {
		return fmt.Errorf("create ssh certificate: %w (if you haven't created a key for your org yet, try `flyctl ssh establish`)", err)
	}

	pemkey := marshalED25519PrivateKey(pk, "single-use certificate")

	terminal.Debugf("Keys for %s configured; connecting...\n", addr)

	sshClient := &ssh.Client{
		Addr: net.JoinHostPort(addr, "22"),
		User: "root",

		Dial: p.Dialer.DialContext,

		Certificate: cert.Certificate,
		PrivateKey:  string(pemkey),
	}

	var endSpin context.CancelFunc
	if !p.DisableSpinner {
		endSpin = spin(fmt.Sprintf("Connecting to %s...", addr),
			fmt.Sprintf("Connecting to %s... complete\n", addr))
		defer endSpin()
	}

	if err := sshClient.Connect(context.Background()); err != nil {
		return errors.Wrap(err, "error connecting to SSH server")
	}
	defer sshClient.Close()

	terminal.Debugf("Connection completed.\n", addr)

	if !p.DisableSpinner {
		endSpin()
	}

	term := &ssh.Terminal{
		Stdin:  p.Stdin,
		Stdout: p.Stdout,
		Stderr: p.Stderr,
		Mode:   "xterm",
	}

	if err := sshClient.Shell(context.Background(), term, p.Cmd, p.Env); err != nil {
		return errors.Wrap(err, "ssh shell")
	}

	return nil
}

// // stolen from `mikesmitty`, thanks, you are a mikesmitty and a scholar
func marshalED25519PrivateKey(key ed25519.PrivateKey, comment string) []byte {
	magic := append([]byte("openssh-key-v1"), 0)

	var w struct {
		CipherName   string
		KdfName      string
		KdfOpts      string
		NumKeys      uint32
		PubKey       []byte
		PrivKeyBlock []byte
	}

	pk1 := struct {
		Check1  uint32
		Check2  uint32
		Keytype string
		Pub     []byte
		Priv    []byte
		Comment string
		Pad     []byte `ssh:"rest"`
	}{}

	ci := rand.Uint32() // skipcq: GSC-G404
	pk1.Check1 = ci
	pk1.Check2 = ci

	pk1.Keytype = sshCrypt.KeyAlgoED25519

	pk := key.Public().(ed25519.PublicKey)
	pubKey := []byte(pk)
	pk1.Pub = pubKey
	pk1.Priv = []byte(key)
	pk1.Comment = comment

	// Add some padding to match the encryption block size within PrivKeyBlock (without Pad field)
	// 8 doesn't match the documentation, but that's what ssh-keygen uses for unencrypted keys. *shrug*
	bs := 8
	blockLen := len(sshCrypt.Marshal(pk1))
	padLen := (bs - (blockLen % bs)) % bs
	pk1.Pad = make([]byte, padLen)

	for i := 0; i < padLen; i++ {
		pk1.Pad[i] = byte(i + 1)
	}

	// Generate the pubkey prefix "\0\0\0\nssh-ed25519\0\0\0 "
	prefix := []byte{0x0, 0x0, 0x0, 0x0b}
	prefix = append(prefix, []byte(sshCrypt.KeyAlgoED25519)...)
	prefix = append(prefix, []byte{0x0, 0x0, 0x0, 0x20}...)

	w.CipherName = "none"
	w.KdfName = "none"
	w.KdfOpts = ""
	w.NumKeys = 1
	w.PubKey = append(w.PubKey, prefix...)
	w.PubKey = append(w.PubKey, pubKey...)
	w.PrivKeyBlock = sshCrypt.Marshal(pk1)

	magic = append(magic, sshCrypt.Marshal(w)...)

	return pem.EncodeToMemory(&pem.Block{
		Type:  "OPENSSH PRIVATE KEY",
		Bytes: magic,
	})
}

func singleUseSSHCertificate(ctx context.Context, org api.OrganizationImpl) (*api.IssuedCertificate, ed25519.PrivateKey, error) {
	client := client.FromContext(ctx).API()
	hours := 1

	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return nil, nil, err
	}

	icert, err := client.IssueSSHCertificate(ctx, org, []string{"root", "fly"}, nil, &hours, pub)
	if err != nil {
		return nil, nil, err
	}

	return icert, priv, nil
}

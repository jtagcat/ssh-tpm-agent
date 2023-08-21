package agent

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/foxboron/ssh-tpm-agent/key"
	"github.com/foxboron/ssh-tpm-agent/signer"
	"github.com/google/go-tpm/tpm2/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var ErrOperationUnsupported = errors.New("operation unsupported")

var (
	SSH_TPM_AGENT_ADD = "tpm-add-key"
)

type Agent struct {
	mu       sync.Mutex
	tpm      func() transport.TPMCloser
	pin      func(*key.Key) ([]byte, error)
	listener *net.UnixListener
	quit     chan interface{}
	wg       sync.WaitGroup
	keys     map[string]*key.Key
	agents   []agent.ExtendedAgent
}

var _ agent.ExtendedAgent = &Agent{}

func (a *Agent) Extension(extensionType string, contents []byte) ([]byte, error) {
	switch extensionType {
	case SSH_TPM_AGENT_ADD:
		return a.AddTPMKey(contents)
	}
	return nil, agent.ErrExtensionUnsupported
}

func (a *Agent) AddTPMKey(contents []byte) ([]byte, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k, err := key.DecodeKey(contents)
	if err != nil {
		fmt.Println(err)
		return nil, err
	}

	sshpubkey, err := k.SSHPublicKey()
	if err != nil {
		return nil, err
	}

	a.keys[ssh.FingerprintSHA256(sshpubkey)] = k

	return []byte(""), nil
}

func (a *Agent) Close() error {
	a.Stop()
	return nil
}

func (a *Agent) signers() ([]ssh.Signer, error) {
	var signers []ssh.Signer

	for _, agent := range a.agents {
		l, err := agent.Signers()
		if err != nil {
			log.Printf("failed getting Signers from agent: %f", err)
			continue
		}
		signers = append(signers, l...)
	}

	for _, k := range a.keys {
		s, err := ssh.NewSignerFromSigner(signer.NewTPMSigner(k, a.tpm, a.pin))
		if err != nil {
			return nil, fmt.Errorf("failed to prepare signer: %w", err)
		}
		signers = append(signers, s)
	}
	return signers, nil
}

func (a *Agent) Signers() ([]ssh.Signer, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.signers()
}

func (a *Agent) List() ([]*agent.Key, error) {
	var agentKeys []*agent.Key

	a.mu.Lock()
	defer a.mu.Unlock()

	for _, agent := range a.agents {
		l, err := agent.List()
		if err != nil {
			log.Printf("failed getting list from agent: %v", err)
			continue
		}
		agentKeys = append(agentKeys, l...)
	}

	for _, k := range a.keys {
		pk, err := k.SSHPublicKey()
		if err != nil {
			return nil, err
		}

		agentKeys = append(agentKeys, &agent.Key{
			Format:  pk.Type(),
			Blob:    pk.Marshal(),
			Comment: string(k.Comment),
		})
	}
	return agentKeys, nil
}

func (a *Agent) SignWithFlags(key ssh.PublicKey, data []byte, flags agent.SignatureFlags) (*ssh.Signature, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	signers, err := a.signers()
	if err != nil {
		return nil, err
	}

	alg := key.Type()
	switch {
	case alg == ssh.KeyAlgoRSA && flags&agent.SignatureFlagRsaSha256 != 0:
		alg = ssh.KeyAlgoRSASHA256
	case alg == ssh.KeyAlgoRSA && flags&agent.SignatureFlagRsaSha512 != 0:
		alg = ssh.KeyAlgoRSASHA512
	}

	for _, s := range signers {
		if !bytes.Equal(s.PublicKey().Marshal(), key.Marshal()) {
			continue
		}
		return s.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, data, alg)
	}

	log.Printf("trying to sign as proxy...")
	for _, agent := range a.agents {
		signers, err := agent.Signers()
		if err != nil {
			log.Printf("failed getting signers from agent: %v", err)
			continue
		}
		for _, s := range signers {
			if !bytes.Equal(s.PublicKey().Marshal(), key.Marshal()) {
				continue
			}
			return s.(ssh.AlgorithmSigner).SignWithAlgorithm(rand.Reader, data, alg)
		}
	}

	return nil, fmt.Errorf("no private keys match the requested public key")
}

func (a *Agent) Sign(key ssh.PublicKey, data []byte) (*ssh.Signature, error) {
	return a.SignWithFlags(key, data, 0)
}

func (a *Agent) serveConn(c net.Conn) {
	if err := agent.ServeAgent(a, c); err != io.EOF {
		log.Println("Agent client connection ended with error:", err)
	}
}

func (a *Agent) Wait() {
	a.wg.Wait()
}

func (a *Agent) Stop() {
	close(a.quit)
	a.listener.Close()
	a.wg.Wait()
}

func (a *Agent) serve() {
	defer a.wg.Done()
	for {
		c, err := a.listener.AcceptUnix()
		if err != nil {
			type temporary interface {
				Temporary() bool
			}
			if err, ok := err.(temporary); ok && err.Temporary() {
				log.Println("Temporary Accept error, sleeping 1s:", err)
				time.Sleep(1 * time.Second)
				continue
			}
			select {
			case <-a.quit:
				return
			default:
				log.Fatalln("Failed to accept connections:", err)
			}
		}
		a.wg.Add(1)
		go func() {
			a.serveConn(c)
			a.wg.Done()
		}()
	}
}

func (a *Agent) AddKey(k *key.Key) error {
	sshpubkey, err := k.SSHPublicKey()
	if err != nil {
		return err
	}
	a.keys[ssh.FingerprintSHA256(sshpubkey)] = k
	return nil
}

func (a *Agent) LoadKeys(keyDir string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	keys, err := LoadKeys(keyDir)
	if err != nil {
		return err
	}

	a.keys = keys
	return nil
}

// Unsupported functions
func (a *Agent) Add(key agent.AddedKey) error {
	return ErrOperationUnsupported
}

func (a *Agent) Remove(key ssh.PublicKey) error {
	return ErrOperationUnsupported
}
func (a *Agent) RemoveAll() error {
	return a.Close()
}
func (a *Agent) Lock(passphrase []byte) error {
	return ErrOperationUnsupported
}
func (a *Agent) Unlock(passphrase []byte) error {
	return ErrOperationUnsupported
}

func LoadKeys(keyDir string) (map[string]*key.Key, error) {
	keys := map[string]*key.Key{}
	err := filepath.WalkDir(keyDir,
		func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, "tpm") {
				return nil
			}
			f, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("failed reading %s", path)
			}
			k, err := key.DecodeKey(f)
			if err != nil {
				log.Printf("%s not a TPM sealed key: %v\n", path, err)
				return nil
			}
			sshpubkey, err := k.SSHPublicKey()
			if err != nil {
				return fmt.Errorf("%s can't read ssh public key from TPM public: %v", path, err)
			}
			keys[ssh.FingerprintSHA256(sshpubkey)] = k
			return nil
		},
	)
	if err != nil {
		log.Fatal(err)
	}
	return keys, nil
}

func NewAgent(listener *net.UnixListener, agents []agent.ExtendedAgent, tpmFetch func() transport.TPMCloser, pin func(*key.Key) ([]byte, error)) *Agent {
	a := &Agent{
		agents:   agents,
		tpm:      tpmFetch,
		listener: listener,
		pin:      pin,
		quit:     make(chan interface{}),
		keys:     make(map[string]*key.Key),
	}

	a.wg.Add(1)
	go a.serve()
	return a
}

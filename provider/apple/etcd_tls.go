// SPDX-License-Identifier: MIT

package apple

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// etcd_tls.go generates and delivers the TLS materials for the managed etcd cluster (v0.5.0),
// closing the "TLS deferred" limit ADR-0003 left open. It is STDLIB-ONLY (crypto/x509 — no
// external CA tooling): one CA, one server cert per etcd member (SAN = member FQDN + localhost +
// 127.0.0.1), and one client cert the k3s servers present. The FQDN SAN is load-bearing — it is
// what keeps TLS verification valid after the vmnet DHCP cold-restart IP shift (the certs name
// the members, never their IPs), the same principle that makes the whole substrate work (ADR-0001).
//
// Delivery reuses the host BIND-MOUNT mechanism (ADR-0001), NOT container cp/exec: the PEMs are
// written under the cluster state dir and bind-mounted read-only into each container. cp/exec ride
// the guest agent (vminitd over vsock), which faults under cold-boot I/O — the same reason the
// kubeconfig and haproxy.cfg are delivered by bind-mount.

const (
	// etcdTLSMount is the in-container path the TLS bundle is bind-mounted at (read-only), for both
	// etcd members and k3s servers. The two mount DIFFERENT host dirs (a member's server bundle vs
	// the shared client bundle) at this same path; the filenames differ so there is no conflict.
	etcdTLSMount = "/etc/etcd/tls"
	// etcdTLSSubdir is the subdir under the cluster state dir holding the generated certs.
	etcdTLSSubdir = "etcd-tls"
	// etcdClientSubdir is the subdir (under etcdTLSSubdir) holding the k3s datastore client bundle.
	etcdClientSubdir = "client"

	etcdCACertFile     = "ca.crt"     // the CA every party trusts (peer + client)
	etcdServerCertFile = "server.crt" // a member's server cert (peer + client-facing)
	etcdServerKeyFile  = "server.key" // a member's server key
	etcdClientCertFile = "client.crt" // the k3s datastore client cert
	etcdClientKeyFile  = "client.key" // the k3s datastore client key
)

// etcdCertValidity is how long the generated certs are valid. These are throwaway per-cluster
// dev certs on a private vmnet, so a long validity avoids a mid-life expiry surprise; the cluster
// is recreated (fresh CA) long before this elapses.
const etcdCertValidity = 10 * 365 * 24 * time.Hour

// certKeyPair is a PEM-encoded certificate and its PEM-encoded private key.
type certKeyPair struct {
	Cert []byte // PEM CERTIFICATE
	Key  []byte // PEM PRIVATE KEY (PKCS#8)
}

// etcdTLSBundle is the full set of PEM materials for the managed etcd cluster: the CA, one server
// cert+key per member (keyed by bare member name), and the single k3s client cert+key.
type etcdTLSBundle struct {
	CAPEM       []byte
	MemberCerts map[string]certKeyPair
	Client      certKeyPair
}

// etcdMemberSANs returns the Subject Alternative Names baked into a member's server cert: the
// member FQDN (<cluster>-etcd-N.<domain> via nodeFQDN) plus localhost / 127.0.0.1. The FQDN SAN
// is the load-bearing one — peers and the k3s client reach the member BY NAME, so the cert stays
// valid across the DHCP IP shift; no vmnet IP is ever a SAN (it would go stale on restart). Pure
// so the SAN contents are unit-testable (BVA: FQDN present, localhost present, loopback present).
func etcdMemberSANs(memberName, domain string) (dnsNames []string, ips []net.IP) {
	return []string{nodeFQDN(memberName, domain), "localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)}
}

// newCertSerial returns a random 128-bit certificate serial number.
func newCertSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating cert serial: %w", err)
	}

	return serial, nil
}

// generateEtcdCA mints a self-signed CA for one cluster's etcd quorum. Returns the parsed cert and
// key (to sign leaves with) plus the CA's PEM (delivered to every party as the trusted-ca file).
func generateEtcdCA(clusterName string) (*x509.Certificate, *ecdsa.PrivateKey, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generating etcd CA key: %w", err)
	}

	serial, err := newCertSerial()
	if err != nil {
		return nil, nil, nil, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: clusterName + "-etcd-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(etcdCertValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating etcd CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parsing etcd CA cert: %w", err)
	}

	return caCert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}

// signEtcdLeaf signs one leaf cert (member server or k3s client) with the CA. dnsNames/ips are the
// SANs (nil for the client cert — it presents identity, not a hostname); eku is the extended key
// usage (ServerAuth+ClientAuth for a member that is both a peer server and a peer client; ClientAuth
// for the k3s client).
func signEtcdLeaf(ca *x509.Certificate, caKey *ecdsa.PrivateKey, commonName string,
	dnsNames []string, ips []net.IP, eku []x509.ExtKeyUsage,
) (certKeyPair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return certKeyPair{}, fmt.Errorf("generating key for %q: %w", commonName, err)
	}

	serial, err := newCertSerial()
	if err != nil {
		return certKeyPair{}, err
	}

	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(etcdCertValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  eku,
		DNSNames:     dnsNames,
		IPAddresses:  ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		return certKeyPair{}, fmt.Errorf("creating cert for %q: %w", commonName, err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return certKeyPair{}, fmt.Errorf("marshaling key for %q: %w", commonName, err)
	}

	return certKeyPair{
		Cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
	}, nil
}

// generateEtcdTLS mints the whole bundle: one CA, one server cert per member (SAN = member FQDN +
// localhost + 127.0.0.1, ExtKeyUsage server+client because a member is both a peer server and a
// peer client), and one k3s client cert (ExtKeyUsage client). Pure (no I/O) so it is unit-testable
// — a test can parse each cert and verify its SANs + chain against the CA without launching a VM.
func generateEtcdTLS(clusterName, domain string, memberNames []string) (etcdTLSBundle, error) {
	ca, caKey, caPEM, err := generateEtcdCA(clusterName)
	if err != nil {
		return etcdTLSBundle{}, err
	}

	members := make(map[string]certKeyPair, len(memberNames))

	for _, m := range memberNames {
		dnsNames, ips := etcdMemberSANs(m, domain)

		pair, err := signEtcdLeaf(ca, caKey, m, dnsNames, ips,
			[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth})
		if err != nil {
			return etcdTLSBundle{}, err
		}

		members[m] = pair
	}

	client, err := signEtcdLeaf(ca, caKey, clusterName+"-etcd-client", nil, nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	if err != nil {
		return etcdTLSBundle{}, err
	}

	return etcdTLSBundle{CAPEM: caPEM, MemberCerts: members, Client: client}, nil
}

// writeEtcdTLS materializes a bundle under <clusterDir>/etcd-tls: one subdir per member holding
// ca.crt/server.crt/server.key, and a client subdir holding ca.crt/client.crt/client.key. Returns
// the ABSOLUTE per-member host dirs (keyed by member name) and the client dir, each ready to
// bind-mount read-only. Absolute paths are required: `container` resolves a relative bind source
// against the container root (same reason the kubeconfig/haproxy mounts are absolute).
//
// All files are written 0600. The etcd/k3s container processes run as guest root, which Apple
// `container` maps to the host user that owns these files (the same mapping that lets k3s, as guest
// root, write the kubeconfig into the host-owned bind-mount), so 0600 is both host-secure and
// guest-readable — no need to widen private keys to 0644.
func writeEtcdTLS(clusterDir string, bundle etcdTLSBundle) (memberDirs map[string]string, clientDir string, err error) {
	root, err := filepath.Abs(filepath.Join(clusterDir, etcdTLSSubdir))
	if err != nil {
		return nil, "", fmt.Errorf("resolving etcd TLS dir: %w", err)
	}

	memberDirs = make(map[string]string, len(bundle.MemberCerts))

	for name, pair := range bundle.MemberCerts {
		dir := filepath.Join(root, name)

		files := map[string][]byte{
			etcdCACertFile:     bundle.CAPEM,
			etcdServerCertFile: pair.Cert,
			etcdServerKeyFile:  pair.Key,
		}
		if err := writeCertFiles(dir, files); err != nil {
			return nil, "", err
		}

		memberDirs[name] = dir
	}

	clientDir = filepath.Join(root, etcdClientSubdir)

	clientFiles := map[string][]byte{
		etcdCACertFile:     bundle.CAPEM,
		etcdClientCertFile: bundle.Client.Cert,
		etcdClientKeyFile:  bundle.Client.Key,
	}
	if err := writeCertFiles(clientDir, clientFiles); err != nil {
		return nil, "", err
	}

	return memberDirs, clientDir, nil
}

// writeCertFiles creates dir and writes each name->PEM file at 0600 (see writeEtcdTLS for why 0600
// is guest-readable). A small helper so the member and client write loops share one I/O path.
func writeCertFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating etcd TLS dir %q: %w", dir, err)
	}

	for name, data := range files {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return fmt.Errorf("writing etcd TLS file %q: %w", path, err)
		}
	}

	return nil
}

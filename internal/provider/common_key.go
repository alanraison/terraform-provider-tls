package provider

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"golang.org/x/crypto/ssh"
)

// keyGenerator extracts data from the given *schema.ResourceData,
// and generates a new public/private key-pair according to the
// selected algorithm.
type keyGenerator func(d *schema.ResourceData) (crypto.PrivateKey, error)

// keyParser parses a private key from the given []byte,
// according to the selected algorithm.
type keyParser func([]byte) (crypto.PrivateKey, error)

// keyGenerators provides a keyGenerator given a specific Algorithm.
var keyGenerators = map[Algorithm]keyGenerator{
	RSA: func(d *schema.ResourceData) (crypto.PrivateKey, error) {
		rsaBits := d.Get("rsa_bits").(int)
		return rsa.GenerateKey(rand.Reader, rsaBits)
	},
	ECDSA: func(d *schema.ResourceData) (crypto.PrivateKey, error) {
		curve := ECDSACurve(d.Get("ecdsa_curve").(string))
		switch curve {
		case P224:
			return ecdsa.GenerateKey(elliptic.P224(), rand.Reader)
		case P256:
			return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		case P384:
			return ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
		case P521:
			return ecdsa.GenerateKey(elliptic.P521(), rand.Reader)
		default:
			return nil, fmt.Errorf("invalid ECDSA curve; supported values are: %v", SupportedECDSACurves())
		}
	},
	ED25519: func(d *schema.ResourceData) (crypto.PrivateKey, error) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, fmt.Errorf("failed to generate ED25519 key: %s", err)
		}
		return key, err
	},
}

// keyParsers provides a keyParser given a specific PEMPreamble.
var keyParsers = map[PEMPreamble]keyParser{
	PreamblePrivateKeyRSA: func(der []byte) (crypto.PrivateKey, error) {
		return x509.ParsePKCS1PrivateKey(der)
	},
	PreamblePrivateKeyEC: func(der []byte) (crypto.PrivateKey, error) {
		return x509.ParseECPrivateKey(der)
	},
	PreamblePrivateKeyPKCS8: func(der []byte) (crypto.PrivateKey, error) {
		return x509.ParsePKCS8PrivateKey(der)
	},
}

// parsePrivateKeyPEM takes a slide of bytes containing a private key
// encoded in [PEM (RFC 1421)](https://datatracker.ietf.org/doc/html/rfc1421) format,
// and returns a crypto.PrivateKey implementation, together with the Algorithm used by the key.
func parsePrivateKeyPEM(keyPEMBytes []byte) (crypto.PrivateKey, Algorithm, error) {
	pemBlock, rest := pem.Decode(keyPEMBytes)
	if pemBlock == nil {
		return nil, "", fmt.Errorf("failed to decode PEM block: decoded bytes %d, undecoded %d", len(keyPEMBytes)-len(rest), len(rest))
	}

	// Identify the PEM preamble from the block
	preamble, err := PEMBlockToPEMPreamble(pemBlock)
	if err != nil {
		return nil, "", err
	}

	// Identify parser for the given PEM preamble
	parser, ok := keyParsers[preamble]
	if !ok {
		return nil, "", fmt.Errorf("unable to determine parser for PEM preamble: %s", preamble)
	}

	// Parse the specific crypto.PrivateKey from the PEM Block bytes
	prvKey, err := parser(pemBlock.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("failed to parse private key given PEM preamble '%s': %w", preamble, err)
	}

	// Identify the Algorithm of the crypto.PrivateKey
	algorithm, err := privateKeyToAlgorithm(prvKey)
	if err != nil {
		return nil, "", fmt.Errorf("failed to determine key algorithm for private key of type %T: %w", prvKey, err)
	}

	return prvKey, algorithm, nil
}

// parsePrivateKeyOpenSSHPEM takes a slide of bytes containing a private key
// encoded in [OpenSSH PEM (RFC 4716)](https://datatracker.ietf.org/doc/html/rfc4716) format,
// and returns a crypto.PrivateKey implementation, together with the Algorithm used by the key.
func parsePrivateKeyOpenSSHPEM(keyOpenSSHPEMBytes []byte) (crypto.PrivateKey, Algorithm, error) {
	prvKey, err := ssh.ParseRawPrivateKey(keyOpenSSHPEMBytes)
	if err != nil {
		return nil, "", fmt.Errorf("failed to parse openssh private key: %w", err)
	}

	algorithm, err := privateKeyToAlgorithm(prvKey)
	if err != nil {
		return nil, "", fmt.Errorf("failed to determine key algorithm for private key of type %T: %w", prvKey, err)
	}

	return prvKey, algorithm, nil
}

// privateKeyToPublicKey takes a crypto.PrivateKey and extracts the corresponding crypto.PublicKey,
// after having figured out its type.
func privateKeyToPublicKey(prvKey crypto.PrivateKey) (crypto.PublicKey, error) {
	signer, ok := prvKey.(crypto.Signer)
	if !ok {
		return nil, fmt.Errorf("unsupported private key type: %T", prvKey)
	}

	return signer.Public(), nil
}

// privateKeyToAlgorithm identifies the Algorithm used by a given crypto.PrivateKey.
func privateKeyToAlgorithm(prvKey crypto.PrivateKey) (Algorithm, error) {
	switch prvKey.(type) {
	case rsa.PrivateKey, *rsa.PrivateKey:
		return RSA, nil
	case ecdsa.PrivateKey, *ecdsa.PrivateKey:
		return ECDSA, nil
	case ed25519.PrivateKey, *ed25519.PrivateKey:
		return ED25519, nil
	default:
		return "", fmt.Errorf("unsupported private key type: %T", prvKey)
	}
}

// setPublicKeyAttributes takes a crypto.PrivateKey, extracts the corresponding crypto.PublicKey and then
// encodes related attributes on the given schema.ResourceData.
func setPublicKeyAttributes(d *schema.ResourceData, prvKey crypto.PrivateKey) diag.Diagnostics {
	pubKey, err := privateKeyToPublicKey(prvKey)
	if err != nil {
		return diag.Errorf("failed to get public key from private key: %v", err)
	}
	pubKeyBytes, err := x509.MarshalPKIXPublicKey(pubKey)
	if err != nil {
		return diag.Errorf("failed to marshal public key: %v", err)
	}
	pubKeyPemBlock := &pem.Block{
		Type:  PreamblePublicKey.String(),
		Bytes: pubKeyBytes,
	}

	d.SetId(hashForState(string(pubKeyBytes)))

	if err := d.Set("public_key_pem", string(pem.EncodeToMemory(pubKeyPemBlock))); err != nil {
		return diag.Errorf("error setting value on key 'public_key_pem': %s", err)
	}

	// NOTE: ECDSA keys with elliptic curve P-224 are not supported by `x/crypto/ssh`,
	// so this will return an error: in that case, we set the below fields to emptry strings
	sshPubKey, err := ssh.NewPublicKey(pubKey)
	var pubKeySSH, pubKeySSHFingerprintMD5, pubKeySSHFingerprintSHA256 string
	if err == nil {
		sshPubKeyBytes := ssh.MarshalAuthorizedKey(sshPubKey)

		pubKeySSH = string(sshPubKeyBytes)
		pubKeySSHFingerprintMD5 = ssh.FingerprintLegacyMD5(sshPubKey)
		pubKeySSHFingerprintSHA256 = ssh.FingerprintSHA256(sshPubKey)
	}

	if err := d.Set("public_key_openssh", pubKeySSH); err != nil {
		return diag.Errorf("error setting value on key 'public_key_openssh': %s", err)
	}

	if err := d.Set("public_key_fingerprint_md5", pubKeySSHFingerprintMD5); err != nil {
		return diag.Errorf("error setting value on key 'public_key_fingerprint_md5': %s", err)
	}

	if err := d.Set("public_key_fingerprint_sha256", pubKeySSHFingerprintSHA256); err != nil {
		return diag.Errorf("error setting value on key 'public_key_fingerprint_sha256': %s", err)
	}

	return nil
}

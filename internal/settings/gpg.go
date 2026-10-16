package settings

import (
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// GPGKey is one valid patrol-gpg-keys-cm entry.
type GPGKey struct {
	// KeyID is the 16-hex-digit (short) key ID, upper case.
	KeyID string
	// Fingerprint is the full fingerprint, upper-case hex.
	Fingerprint string
	// Owner is the primary identity ("Name (comment) <email>").
	Owner string
	// SubType describes the algorithm and size, gpg style (for example "rsa4096").
	SubType string
	// KeyData is the ASCII-armored public key as configured.
	KeyData string
}

const armoredPublicKeyHeader = "-----BEGIN PGP PUBLIC KEY BLOCK-----"

// GPGKeyID returns the short key ID for a short (16 hex digits) or long (40 hex digits,
// fingerprint) key ID, as upstream sourceintegrity.KeyID: a long ID yields its last 16
// digits. The case is preserved.
func GPGKeyID(k string) (string, error) {
	isHex := func(s string) bool { _, err := hex.DecodeString(s); return err == nil }
	switch {
	case len(k) == 40 && isHex(k):
		return k[24:], nil
	case len(k) == 16 && isHex(k):
		return k, nil
	}
	return "", fmt.Errorf("'%s' is not a valid GnuPG key ID", k)
}

// ParseGPGPublicKey parses exactly one ASCII-armored public key.
func ParseGPGPublicKey(armored string) (GPGKey, error) {
	if n := strings.Count(armored, armoredPublicKeyHeader); n > 1 {
		return GPGKey{}, errors.New("more than one key found in input data")
	}
	entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armored))
	if err != nil {
		return GPGKey{}, fmt.Errorf("error validating PGP key: %w", err)
	}
	switch {
	case len(entities) == 0:
		return GPGKey{}, errors.New("could not find the GPG key")
	case len(entities) > 1:
		return GPGKey{}, errors.New("more than one key found in input data")
	}
	e := entities[0]
	if e.PrivateKey != nil {
		return GPGKey{}, errors.New("private key material must not be stored in the GPG keys ConfigMap")
	}
	pk := e.PrimaryKey
	key := GPGKey{
		KeyID:       pk.KeyIdString(),
		Fingerprint: strings.ToUpper(hex.EncodeToString(pk.Fingerprint)),
		SubType:     gpgSubType(pk),
		KeyData:     armored,
	}
	if id := e.PrimaryIdentity(); id != nil {
		key.Owner = id.Name
	}
	return key, nil
}

func gpgSubType(pk *packet.PublicKey) string {
	bits, err := pk.BitLength()
	size := ""
	if err == nil && bits > 0 {
		size = fmt.Sprint(bits)
	}
	switch pk.PubKeyAlgo {
	case packet.PubKeyAlgoRSA, packet.PubKeyAlgoRSASignOnly, packet.PubKeyAlgoRSAEncryptOnly:
		return "rsa" + size
	case packet.PubKeyAlgoDSA:
		return "dsa" + size
	case packet.PubKeyAlgoECDSA:
		return "nistp" + size
	case packet.PubKeyAlgoEdDSA, packet.PubKeyAlgoEd25519:
		return "ed25519"
	case packet.PubKeyAlgoEd448:
		return "ed448"
	}
	return fmt.Sprintf("algo%d", pk.PubKeyAlgo)
}

// parseGPGKeys reads patrol-gpg-keys-cm. Each key must be a valid key ID and its value must
// hold exactly one public key with that ID (upstream wording). Invalid entries are dropped
// and reported; the others stay usable.
func (p *parser) parseGPGKeys() {
	src := SourceGPGKeysConfigMap
	data := p.cmData(src)
	for _, k := range slices.Sorted(maps.Keys(data)) {
		expected, err := GPGKeyID(k)
		if err != nil {
			p.errorf(src, k, "error validating key ID %q in GPG keys configmap: %v; the entry is ignored", k, err)
			continue
		}
		key, err := ParseGPGPublicKey(data[k])
		if err != nil {
			p.errorf(src, k, "error parsing GPG key for entry %q: %v; the entry is ignored", expected, err)
			continue
		}
		if key.KeyID != expected {
			p.errorf(src, k, "key parsed for entry with key ID '%s' had different key ID '%s'; the entry is ignored", expected, key.KeyID)
			continue
		}
		p.s.gpgKeys[key.KeyID] = key
		p.accept(src, k, data[k])
	}
}

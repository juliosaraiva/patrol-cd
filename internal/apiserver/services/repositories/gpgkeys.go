package repositories

import (
	"bytes"
	"context"
	"maps"
	"slices"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"

	"github.com/juliosaraiva/patrol-cd/internal/apiserver/rest"
	"github.com/juliosaraiva/patrol-cd/internal/settings"
	"github.com/juliosaraiva/patrol-cd/pkg/apiclient"
)

const (
	pgpPublicBegin  = "-----BEGIN PGP PUBLIC KEY BLOCK-----"
	pgpPublicEnd    = "-----END PGP PUBLIC KEY BLOCK-----"
	pgpPrivateBlock = "PGP PRIVATE KEY BLOCK"
	// gpgTrust is the trust the repo-server gives every configured key (REPO-064).
	gpgTrust = "ultimate"
	// maxGPGKeysPerRequest bounds the keys of one create request.
	maxGPGKeysPerRequest = 256
)

func gpgKeyToAPI(k settings.GPGKey, withData bool) apiclient.GnuPGPublicKey {
	out := apiclient.GnuPGPublicKey{KeyID: k.KeyID, Fingerprint: k.Fingerprint, Owner: k.Owner, Trust: gpgTrust, SubType: k.SubType}
	if withData {
		out.KeyData = k.KeyData
	}
	return out
}

// parseConfiguredKeys parses the entries of patrol-gpg-keys-cm; invalid entries are
// skipped (the settings layer drops them too) and logged.
func (s *Service) parseConfiguredKeys(ctx context.Context, data map[string]string) map[string]settings.GPGKey {
	out := map[string]settings.GPGKey{}
	for _, id := range slices.Sorted(maps.Keys(data)) {
		k, err := settings.ParseGPGPublicKey(data[id])
		if err != nil || !strings.EqualFold(k.KeyID, id) {
			s.log.WarnContext(ctx, "ignoring an invalid GPG keys ConfigMap entry", "keyID", id)
			continue
		}
		out[strings.ToUpper(id)] = k
	}
	return out
}

// normalizeKeyID accepts a short key ID or a fingerprint and returns the upper-case short
// ID.
func normalizeKeyID(id string) (string, error) {
	short, err := settings.GPGKeyID(id)
	if err != nil {
		return "", rest.InvalidArgument("%s", err.Error())
	}
	return strings.ToUpper(short), nil
}

// splitArmoredKeys parses one or more ASCII-armored public key blocks (each possibly a
// key ring) into single-key armored blocks. Private key material is refused.
func splitArmoredKeys(data string) ([]settings.GPGKey, error) {
	if strings.Contains(data, pgpPrivateBlock) {
		return nil, rest.InvalidArgument("private key material must not be submitted; send public keys only")
	}
	var out []settings.GPGKey
	remaining := data
	for {
		i := strings.Index(remaining, pgpPublicBegin)
		if i < 0 {
			break
		}
		j := strings.Index(remaining[i:], pgpPublicEnd)
		if j < 0 {
			return nil, invalid("error validating PGP key: unterminated public key block")
		}
		block := remaining[i : i+j+len(pgpPublicEnd)]
		remaining = remaining[i+j+len(pgpPublicEnd):]
		entities, err := openpgp.ReadArmoredKeyRing(strings.NewReader(block))
		if err != nil {
			return nil, invalid("error validating PGP key: %v", err)
		}
		for _, e := range entities {
			if e.PrivateKey != nil {
				return nil, invalid("private key material must not be submitted; send public keys only")
			}
			var buf bytes.Buffer
			w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
			if err != nil {
				return nil, invalid("error validating PGP key: %v", err)
			}
			if err := e.Serialize(w); err != nil {
				return nil, invalid("error validating PGP key: %v", err)
			}
			if err := w.Close(); err != nil {
				return nil, invalid("error validating PGP key: %v", err)
			}
			buf.WriteByte('\n')
			k, err := settings.ParseGPGPublicKey(buf.String())
			if err != nil {
				return nil, invalid("error validating PGP key: %v", err)
			}
			out = append(out, k)
			if len(out) > maxGPGKeysPerRequest {
				return nil, invalid("more than %d keys in one request", maxGPGKeysPerRequest)
			}
		}
	}
	if len(out) == 0 {
		return nil, invalid("error validating PGP key: no ASCII-armored public key found")
	}
	return out, nil
}

func (s *Service) listGPGKeys(ctx context.Context, _ *apiclient.Empty) (*apiclient.GnuPGPublicKeyList, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	t, err := s.readTrustConfigMap(ctx, kc, s.cfg.GPGKeysConfigMap)
	if err != nil {
		return nil, err
	}
	keys := s.parseConfiguredKeys(ctx, t.data())
	out := &apiclient.GnuPGPublicKeyList{Items: []apiclient.GnuPGPublicKey{}}
	for _, id := range slices.Sorted(maps.Keys(keys)) {
		out.Items = append(out.Items, gpgKeyToAPI(keys[id], false))
	}
	return out, nil
}

func (s *Service) getGPGKey(ctx context.Context, req *apiclient.GnuPGPublicKeyQuery) (*apiclient.GnuPGPublicKey, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	id, err := normalizeKeyID(req.KeyID)
	if err != nil {
		return nil, err
	}
	t, err := s.readTrustConfigMap(ctx, kc, s.cfg.GPGKeysConfigMap)
	if err != nil {
		return nil, err
	}
	k, ok := s.parseConfiguredKeys(ctx, t.data())[id]
	if !ok {
		return nil, rest.NotFound("no such key: %s", id)
	}
	out := gpgKeyToAPI(k, true)
	return &out, nil
}

func (s *Service) createGPGKeys(ctx context.Context, req *apiclient.GnuPGPublicKeyCreateRequest) (*apiclient.GnuPGPublicKeyCreateResponse, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	data := strings.TrimSpace(req.Body.KeyData)
	if data == "" {
		return nil, rest.InvalidArgument("submitted key data is empty")
	}
	keys, err := splitArmoredKeys(data)
	if err != nil {
		return nil, err
	}
	out := &apiclient.GnuPGPublicKeyCreateResponse{Created: apiclient.GnuPGPublicKeyList{Items: []apiclient.GnuPGPublicKey{}}}
	err = retryOnConflict(func() error {
		out.Created.Items, out.Skipped = []apiclient.GnuPGPublicKey{}, nil
		t, err := s.readTrustConfigMap(ctx, kc, s.cfg.GPGKeysConfigMap)
		if err != nil {
			return err
		}
		cm := t.data()
		existing := map[string]bool{}
		for id := range cm {
			existing[strings.ToUpper(id)] = true
		}
		changed := false
		for _, k := range keys {
			id := strings.ToUpper(k.KeyID)
			if existing[id] {
				if !slices.Contains(out.Skipped, id) {
					out.Skipped = append(out.Skipped, id)
				}
				continue
			}
			existing[id] = true
			cm[id] = k.KeyData
			changed = true
			out.Created.Items = append(out.Created.Items, gpgKeyToAPI(k, false))
		}
		if !changed {
			return nil
		}
		return s.writeTrustConfigMap(ctx, kc, t, cm)
	})
	if err != nil {
		return nil, err
	}
	rest.AuditFrom(ctx).SetMessage("added %d GnuPG public keys", len(out.Created.Items))
	return out, nil
}

func (s *Service) deleteGPGKey(ctx context.Context, req *apiclient.GnuPGPublicKeyQuery) (*apiclient.Empty, error) {
	kc, err := s.kube()
	if err != nil {
		return nil, err
	}
	id, err := normalizeKeyID(req.KeyID)
	if err != nil {
		return nil, err
	}
	err = retryOnConflict(func() error {
		t, err := s.readTrustConfigMap(ctx, kc, s.cfg.GPGKeysConfigMap)
		if err != nil {
			return err
		}
		cm := t.data()
		found := ""
		for k := range cm {
			if strings.EqualFold(k, id) {
				found = k
				break
			}
		}
		if found == "" {
			return rest.NotFound("no such key configured: %s", id)
		}
		delete(cm, found)
		return s.writeTrustConfigMap(ctx, kc, t, cm)
	})
	if err != nil {
		return nil, err
	}
	rest.AuditFrom(ctx).SetMessage("removed GnuPG public key %s", id)
	return &apiclient.Empty{}, nil
}

package mirror

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	pgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/grmrgecko/repo-sync/fetch"
)

// SignatureMode controls OpenPGP verification of repository metadata.
type SignatureMode string

const (
	// SignatureOff mirrors signatures without checking them.
	SignatureOff SignatureMode = "off"
	// SignatureIfPresent accepts unsigned repositories but verifies every
	// signature the upstream publishes.
	SignatureIfPresent SignatureMode = "if-present"
	// SignatureRequired refuses repository metadata without a valid signature.
	SignatureRequired SignatureMode = "required"
)

// ParseSignatureMode validates a signature policy name.
func ParseSignatureMode(value string) (SignatureMode, error) {
	mode := SignatureMode(strings.ToLower(strings.TrimSpace(value)))
	switch mode {
	case SignatureOff, SignatureIfPresent, SignatureRequired:
		return mode, nil
	default:
		return "", fmt.Errorf("unsupported signature mode %q", value)
	}
}

// signatureVerifier verifies detached signatures with local, repository, and
// optionally keyserver-provided public keys.
type signatureVerifier struct {
	keys       openpgp.EntityList
	keyservers []string
	pinned     bool
}

// legacySignature is the parsed subset of an OpenPGP version 3 detached RSA
// signature needed by CentOS 7-era repositories.
type legacySignature struct {
	issuer    uint64
	hash      crypto.Hash
	hashTag   [2]byte
	hashed    []byte
	signature []byte
}

// newSignatureVerifier loads the operator-configured public keys.
func newSignatureVerifier(opts *Options) (*signatureVerifier, error) {
	v := &signatureVerifier{keyservers: opts.Keyservers, pinned: len(opts.GPGKeys) > 0 || len(opts.GPGKeyData) > 0}
	for _, data := range opts.GPGKeyData {
		keys, err := readPublicKeys(data)
		if err != nil {
			return nil, fmt.Errorf("parse configured GPG key: %w", err)
		}
		v.keys = append(v.keys, keys...)
	}
	for _, name := range opts.GPGKeys {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, fmt.Errorf("read GPG key %s: %w", name, err)
		}
		keys, err := readPublicKeys(data)
		if err != nil {
			return nil, fmt.Errorf("parse GPG key %s: %w", name, err)
		}
		v.keys = append(v.keys, keys...)
	}
	return v, nil
}

// readPublicKeys accepts armored and binary OpenPGP public keyrings.
func readPublicKeys(data []byte) (openpgp.EntityList, error) {
	if bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN PGP")) {
		return openpgp.ReadArmoredKeyRing(bytes.NewReader(data))
	}
	return openpgp.ReadKeyRing(bytes.NewReader(data))
}

// decodedSignature returns the packet stream from an armored or binary
// detached signature.
func decodedSignature(data []byte) ([]byte, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(data), []byte("-----BEGIN PGP")) {
		return data, nil
	}
	block, err := pgparmor.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if block.Type != openpgp.SignatureType {
		return nil, fmt.Errorf("armor contains %q instead of a signature", block.Type)
	}
	return io.ReadAll(block.Body)
}

// parseLegacySignature parses the version 3 signature packet format emitted
// by GnuPG v1. Modern signature packets return ok=false for normal handling.
func parseLegacySignature(data []byte) (sig legacySignature, ok bool, err error) {
	opaque, err := packet.NewOpaqueReader(bytes.NewReader(data)).Next()
	if err != nil {
		return sig, false, err
	}
	if opaque.Tag != 2 || len(opaque.Contents) == 0 || opaque.Contents[0] != 3 {
		return sig, false, nil
	}
	body := opaque.Contents
	if len(body) < 21 || body[1] != 5 {
		return sig, true, errors.New("invalid OpenPGP version 3 signature packet")
	}
	if body[2] != byte(packet.SigTypeBinary) {
		return sig, true, fmt.Errorf("unsupported OpenPGP version 3 signature type %d", body[2])
	}
	if body[15] != byte(packet.PubKeyAlgoRSA) && body[15] != byte(packet.PubKeyAlgoRSASignOnly) {
		return sig, true, fmt.Errorf("unsupported OpenPGP version 3 public key algorithm %d", body[15])
	}
	hash, supported := openpgp.HashIdToHash(body[16])
	if !supported || !hash.Available() {
		return sig, true, fmt.Errorf("unsupported OpenPGP version 3 hash algorithm %d", body[16])
	}
	bits := int(binary.BigEndian.Uint16(body[19:21]))
	bytesLen := (bits + 7) / 8
	if bytesLen == 0 || len(body) != 21+bytesLen {
		return sig, true, errors.New("invalid OpenPGP version 3 RSA signature")
	}
	sig.issuer = binary.BigEndian.Uint64(body[7:15])
	sig.hash = hash
	copy(sig.hashTag[:], body[17:19])
	sig.hashed = append([]byte(nil), body[2:7]...)
	sig.signature = append([]byte(nil), body[21:]...)
	return sig, true, nil
}

// verifyLegacySignature verifies a version 3 binary-document RSA signature.
func verifyLegacySignature(keys openpgp.EntityList, data []byte, sig legacySignature) (*openpgp.Entity, error) {
	h := sig.hash.New()
	if _, err := h.Write(data); err != nil {
		return nil, err
	}
	if _, err := h.Write(sig.hashed); err != nil {
		return nil, err
	}
	digest := h.Sum(nil)
	if !bytes.Equal(digest[:2], sig.hashTag[:]) {
		return nil, errors.New("OpenPGP signature hash tag does not match")
	}
	for _, entity := range keys {
		publicKeys := []*packet.PublicKey{entity.PrimaryKey}
		for _, subkey := range entity.Subkeys {
			publicKeys = append(publicKeys, subkey.PublicKey)
		}
		for _, key := range publicKeys {
			if key == nil || key.KeyId != sig.issuer {
				continue
			}
			pub, ok := key.PublicKey.(*rsa.PublicKey)
			if !ok {
				continue
			}
			signature := sig.signature
			if size := pub.Size(); len(signature) < size {
				padded := make([]byte, size)
				copy(padded[size-len(signature):], signature)
				signature = padded
			}
			if err := rsa.VerifyPKCS1v15(pub, sig.hash, digest, signature); err == nil {
				return entity, nil
			}
		}
	}
	return nil, pgperrors.ErrUnknownIssuer
}

// signatureLookup identifies the signer requested by the first signature
// packet. Full fingerprints are preferred over 64-bit key IDs.
func signatureLookup(data []byte) (string, error) {
	decoded, err := decodedSignature(data)
	if err != nil {
		return "", err
	}
	legacy, ok, err := parseLegacySignature(decoded)
	if err != nil {
		return "", err
	}
	if ok {
		return fmt.Sprintf("%016X", legacy.issuer), nil
	}
	p, err := packet.NewReader(bytes.NewReader(decoded)).Next()
	if err != nil {
		return "", err
	}
	sig, ok := p.(*packet.Signature)
	if !ok {
		return "", errors.New("detached signature contains no signature packet")
	}
	if len(sig.IssuerFingerprint) > 0 {
		return strings.ToUpper(hex.EncodeToString(sig.IssuerFingerprint)), nil
	}
	if sig.IssuerKeyId != nil {
		return fmt.Sprintf("%016X", *sig.IssuerKeyId), nil
	}
	return "", errors.New("detached signature has no issuer")
}

// keyserverLookupURL builds a Hockeypuck-compatible exact key lookup.
func keyserverLookupURL(base, issuer string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("invalid keyserver URL %q", base)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/pks/lookup"
	q := u.Query()
	q.Set("op", "get")
	q.Set("options", "mr")
	q.Set("search", "0x"+issuer)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// retrieveSigner asks configured keyservers for the signature issuer and
// ignores responses whose keys do not verify the pair.
func (v *signatureVerifier) retrieveSigner(
	ctx context.Context,
	signature []byte,
	base openpgp.EntityList,
	verify func(openpgp.EntityList) (*openpgp.Entity, error),
) (*openpgp.Entity, error) {
	issuer, err := signatureLookup(signature)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, server := range v.keyservers {
		lookup, err := keyserverLookupURL(server, issuer)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		data, err := fetch.ReadURL(ctx, lookup)
		if err != nil {
			errs = append(errs, fmt.Errorf("fetch key %s from %s: %w", issuer, server, err))
			continue
		}
		keys, err := readPublicKeys(data)
		if err != nil {
			errs = append(errs, fmt.Errorf("parse key %s from %s: %w", issuer, server, err))
			continue
		}
		candidate := append(append(openpgp.EntityList(nil), base...), keys...)
		signer, err := verify(candidate)
		if err != nil {
			errs = append(errs, fmt.Errorf("verify key %s from %s: %w", issuer, server, err))
			continue
		}
		v.keys = append(v.keys, keys...)
		return signer, nil
	}
	if len(errs) == 0 {
		return nil, fmt.Errorf("signature issuer %s is unknown; configure a GPG key or keyserver", issuer)
	}
	return nil, errors.Join(errs...)
}

// verifyDetachedBytes verifies a detached signature over data and returns the
// matching primary-key fingerprint.
func (v *signatureVerifier) verifyDetachedBytes(ctx context.Context, data, signature, repoKey []byte) (string, error) {
	keys := append(openpgp.EntityList(nil), v.keys...)
	if len(repoKey) > 0 && !v.pinned {
		repoKeys, err := readPublicKeys(repoKey)
		if err != nil {
			return "", fmt.Errorf("parse repository GPG key: %w", err)
		}
		keys = append(keys, repoKeys...)
	}

	decoded, err := decodedSignature(signature)
	if err != nil {
		return "", err
	}
	legacy, legacyOK, err := parseLegacySignature(decoded)
	if err != nil {
		return "", err
	}
	verify := func(keyring openpgp.EntityList) (*openpgp.Entity, error) {
		if legacyOK {
			return verifyLegacySignature(keyring, data, legacy)
		}
		_, signer, err := openpgp.VerifyDetachedSignature(keyring, bytes.NewReader(data), bytes.NewReader(decoded), nil)
		return signer, err
	}

	signer, err := verify(keys)
	if errors.Is(err, pgperrors.ErrUnknownIssuer) && len(v.keyservers) > 0 && !v.pinned {
		signer, err = v.retrieveSigner(ctx, signature, keys, verify)
	}
	if err != nil {
		if errors.Is(err, pgperrors.ErrUnknownIssuer) {
			issuer, lookupErr := signatureLookup(signature)
			if lookupErr == nil {
				if v.pinned {
					return "", fmt.Errorf("signature issuer %s is not present in the configured GPG keys", issuer)
				}
				return "", fmt.Errorf("signature issuer %s is unknown; configure a GPG key or keyserver", issuer)
			}
		}
		return "", err
	}
	return strings.ToUpper(hex.EncodeToString(signer.PrimaryKey.Fingerprint)), nil
}

// verifyDetached verifies sigPath against the exact bytes at dataPath and
// returns the matching primary-key fingerprint.
func (v *signatureVerifier) verifyDetached(ctx context.Context, dataPath, sigPath, repoKeyPath string) (string, error) {
	data, err := os.ReadFile(dataPath)
	if err != nil {
		return "", err
	}
	signature, err := os.ReadFile(sigPath)
	if err != nil {
		return "", err
	}
	var repoKey []byte
	if repoKeyPath != "" {
		repoKey, err = os.ReadFile(repoKeyPath)
		if err != nil {
			return "", err
		}
	}
	return v.verifyDetachedBytes(ctx, data, signature, repoKey)
}

// verifyClearsigned verifies an OpenPGP cleartext message and returns its
// authenticated plaintext and signer fingerprint.
func (v *signatureVerifier) verifyClearsigned(ctx context.Context, data []byte) ([]byte, string, error) {
	block, rest := clearsign.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, "", errors.New("invalid OpenPGP clearsigned message")
	}
	signature, err := io.ReadAll(block.ArmoredSignature.Body)
	if err != nil {
		return nil, "", err
	}
	fingerprint, err := v.verifyDetachedBytes(ctx, block.Bytes, signature, nil)
	if err != nil {
		return nil, "", err
	}
	return block.Plaintext, fingerprint, nil
}

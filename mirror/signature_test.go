package mirror

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestVerifyLegacyDetachedSignature locks the OpenPGP version 3 RSA packet
// shape used by GnuPG v1 to sign archived CentOS 7 repomd.xml files.
func TestVerifyLegacyDetachedSignature(t *testing.T) {
	entity, err := openpgp.NewEntity("CentOS fixture", "", "security@example.com", nil)
	require.NoError(t, err)
	privateKey, ok := entity.PrivateKey.PrivateKey.(*rsa.PrivateKey)
	require.True(t, ok)

	data := []byte("signed repository metadata\n")
	hashed := make([]byte, 5)
	hashed[0] = byte(packet.SigTypeBinary)
	binary.BigEndian.PutUint32(hashed[1:], 1678292750)
	h := crypto.SHA256.New()
	_, _ = h.Write(data)
	_, _ = h.Write(hashed)
	digest := h.Sum(nil)
	rsaSignature, err := rsa.SignPKCS1v15(rand.Reader, privateKey, crypto.SHA256, digest)
	require.NoError(t, err)
	mpi := new(big.Int).SetBytes(rsaSignature)
	mpiBytes := mpi.Bytes()

	body := []byte{3, 5}
	body = append(body, hashed...)
	body = binary.BigEndian.AppendUint64(body, entity.PrimaryKey.KeyId)
	body = append(body, byte(packet.PubKeyAlgoRSA), 8, digest[0], digest[1])
	body = binary.BigEndian.AppendUint16(body, uint16(mpi.BitLen()))
	body = append(body, mpiBytes...)
	var packetData bytes.Buffer
	require.NoError(t, (&packet.OpaquePacket{Tag: 2, Contents: body}).Serialize(&packetData))
	var signature bytes.Buffer
	armoredSignature, err := armor.Encode(&signature, openpgp.SignatureType, nil)
	require.NoError(t, err)
	_, err = armoredSignature.Write(packetData.Bytes())
	require.NoError(t, err)
	require.NoError(t, armoredSignature.Close())

	var publicKey bytes.Buffer
	armoredKey, err := armor.Encode(&publicKey, openpgp.PublicKeyType, nil)
	require.NoError(t, err)
	require.NoError(t, entity.Serialize(armoredKey))
	require.NoError(t, armoredKey.Close())

	dir := t.TempDir()
	dataPath := filepath.Join(dir, "repomd.xml")
	sigPath := dataPath + ".asc"
	keyPath := dataPath + ".key"
	require.NoError(t, os.WriteFile(dataPath, data, 0644))
	require.NoError(t, os.WriteFile(sigPath, signature.Bytes(), 0644))
	require.NoError(t, os.WriteFile(keyPath, publicKey.Bytes(), 0644))

	verifier := &signatureVerifier{}
	fingerprint, err := verifier.verifyDetached(context.Background(), dataPath, sigPath, keyPath)
	require.NoError(t, err)
	assert.NotEmpty(t, fingerprint)

	require.NoError(t, os.WriteFile(dataPath, []byte("modified repository metadata\n"), 0644))
	_, err = verifier.verifyDetached(context.Background(), dataPath, sigPath, keyPath)
	assert.Error(t, err)
}

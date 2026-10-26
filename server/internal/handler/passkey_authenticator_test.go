package handler

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/go-webauthn/webauthn/protocol/webauthncbor"
	"github.com/go-webauthn/webauthn/protocol/webauthncose"
)

// fakeAuthenticator is a software passkey for tests: an ES256 key pair, a
// credential id and a signature counter. It answers the options our begin
// endpoints return the way a browser + platform authenticator would, and
// lets a test tamper with the origin, RP ID or challenge it signs over.
type fakeAuthenticator struct {
	key        *ecdsa.PrivateKey
	credID     []byte
	userHandle []byte
	counter    uint32
}

func newFakeAuthenticator(t *testing.T) *fakeAuthenticator {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	id := make([]byte, 16)
	_, _ = rand.Read(id)
	return &fakeAuthenticator{key: key, credID: id}
}

var b64 = base64.RawURLEncoding

// ceremony is the part of a begin response the authenticator needs.
type ceremony struct {
	ChallengeID string `json:"challenge_id"`
	Options     struct {
		PublicKey struct {
			Challenge string `json:"challenge"`
			RPID      string `json:"rpId"`
			RP        struct {
				ID string `json:"id"`
			} `json:"rp"`
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"publicKey"`
	} `json:"options"`
}

// signing describes what the authenticator claims to have seen. Zero values
// mean "what an honest browser at the configured origin would send".
type signing struct {
	origin    string
	rpID      string
	challenge string
}

func (a *fakeAuthenticator) authData(t *testing.T, rpID string, attested bool) []byte {
	t.Helper()
	rpHash := sha256.Sum256([]byte(rpID))
	flags := byte(0x01 | 0x04) // user present + user verified
	if attested {
		flags |= 0x40
	}
	out := append([]byte{}, rpHash[:]...)
	out = append(out, flags)
	out = binary.BigEndian.AppendUint32(out, a.counter)
	if !attested {
		return out
	}
	pub, err := webauthncbor.Marshal(webauthncose.EC2PublicKeyData{
		PublicKeyData: webauthncose.PublicKeyData{KeyType: int64(webauthncose.EllipticKey), Algorithm: int64(webauthncose.AlgES256)},
		Curve:         1,
		XCoord:        a.key.PublicKey.X.FillBytes(make([]byte, 32)),
		YCoord:        a.key.PublicKey.Y.FillBytes(make([]byte, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	out = append(out, make([]byte, 16)...) // AAGUID
	out = binary.BigEndian.AppendUint16(out, uint16(len(a.credID)))
	out = append(out, a.credID...)
	return append(out, pub...)
}

func clientData(t *testing.T, typ, challenge, origin string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"type": typ, "challenge": challenge, "origin": origin, "crossOrigin": false})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (s signing) fill(c ceremony, rpID string) signing {
	if s.origin == "" {
		s.origin = testPasskeyOrigin
	}
	if s.rpID == "" {
		s.rpID = rpID
	}
	if s.challenge == "" {
		s.challenge = c.Options.PublicKey.Challenge
	}
	return s
}

// register answers a creation ceremony with a "none" attestation.
func (a *fakeAuthenticator) register(t *testing.T, c ceremony, s signing) json.RawMessage {
	t.Helper()
	s = s.fill(c, c.Options.PublicKey.RP.ID)
	handle, err := b64.DecodeString(c.Options.PublicKey.User.ID)
	if err != nil {
		t.Fatalf("user handle: %v", err)
	}
	a.userHandle = handle
	att, err := webauthncbor.Marshal(map[string]any{
		"fmt":      "none",
		"attStmt":  map[string]any{},
		"authData": a.authData(t, s.rpID, true),
	})
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id":    b64.EncodeToString(a.credID),
		"rawId": b64.EncodeToString(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"attestationObject": b64.EncodeToString(att),
			"clientDataJSON":    b64.EncodeToString(clientData(t, "webauthn.create", s.challenge, s.origin)),
		},
	})
	return body
}

// assert answers a request ceremony, bumping the signature counter.
func (a *fakeAuthenticator) assert(t *testing.T, c ceremony, s signing) json.RawMessage {
	t.Helper()
	s = s.fill(c, c.Options.PublicKey.RPID)
	a.counter++
	authData := a.authData(t, s.rpID, false)
	cd := clientData(t, "webauthn.get", s.challenge, s.origin)
	cdHash := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, authData...), cdHash[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id":    b64.EncodeToString(a.credID),
		"rawId": b64.EncodeToString(a.credID),
		"type":  "public-key",
		"response": map[string]any{
			"authenticatorData": b64.EncodeToString(authData),
			"clientDataJSON":    b64.EncodeToString(cd),
			"signature":         b64.EncodeToString(sig),
			"userHandle":        b64.EncodeToString(a.userHandle),
		},
	})
	return body
}

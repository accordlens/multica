// Package passkey holds the deployment-level pieces of passkey-only sign-in
// for people (ACCO-55): the relying-party configuration derived from the
// app's public origin and the one-time enrollment links that let a person
// register their first passkey. The HTTP ceremonies live in the handler
// package; the host-side enrollment command lives in cmd/passkey-enroll.
package passkey

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/auth"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"golang.org/x/net/publicsuffix"
)

const (
	// ChallengeTTL bounds how long a begun ceremony can be finished.
	ChallengeTTL = 5 * time.Minute
	// EnrollmentTTL bounds how long a one-time enrollment link stays usable.
	EnrollmentTTL = 24 * time.Hour
	// FreshAuthWindow is how recently a session must have signed in with a
	// passkey before it may register another one.
	FreshAuthWindow = 10 * time.Minute
)

// Config is the relying party a deployment presents to authenticators.
type Config struct {
	RPID   string
	Origin string
}

// ConfigFromAppURL derives the relying party from the browser app's origin
// (MULTICA_APP_URL). rpIDOverride may name a registrable parent domain of
// that host; it is empty in the common case. Only https origins, or plain
// http on localhost for development, are accepted — browsers refuse WebAuthn
// anywhere else, so a misconfiguration fails at startup instead of at the
// first sign-in.
func ConfigFromAppURL(appURL, rpIDOverride string) (Config, error) {
	u, err := url.Parse(strings.TrimSpace(appURL))
	if err != nil || u.Hostname() == "" {
		return Config{}, fmt.Errorf("passkey: MULTICA_APP_URL must be an absolute origin, got %q", appURL)
	}
	host := strings.ToLower(u.Hostname())
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(appURL, "#") {
		return Config{}, errors.New("passkey: MULTICA_APP_URL must not contain credentials, a query or a fragment")
	}
	if net.ParseIP(host) != nil || (host != "localhost" && !strings.Contains(host, ".")) {
		return Config{}, errors.New("passkey: MULTICA_APP_URL must name a DNS host")
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return Config{}, errors.New("passkey: MULTICA_APP_URL has an invalid DNS host")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return Config{}, errors.New("passkey: MULTICA_APP_URL has an invalid DNS host")
			}
		}
	}
	port := u.Port()
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return Config{}, errors.New("passkey: MULTICA_APP_URL has an invalid port")
		}
	} else if strings.HasSuffix(u.Host, ":") {
		return Config{}, errors.New("passkey: MULTICA_APP_URL has an empty port")
	}
	switch {
	case u.Scheme == "https":
	case u.Scheme == "http" && host == "localhost":
	default:
		return Config{}, fmt.Errorf("passkey: MULTICA_APP_URL must use https (or http://localhost), got %q", appURL)
	}
	if u.Path != "" && u.Path != "/" {
		return Config{}, fmt.Errorf("passkey: MULTICA_APP_URL must be an origin without a path, got %q", appURL)
	}

	rpID := host
	if o := strings.ToLower(strings.TrimSpace(rpIDOverride)); o != "" {
		if host != o && !strings.HasSuffix(host, "."+o) {
			return Config{}, fmt.Errorf("passkey: MULTICA_PASSKEY_RP_ID %q is not %q or a parent domain of it", o, host)
		}
		if suffix, _ := publicsuffix.PublicSuffix(o); host != o && suffix == o {
			return Config{}, errors.New("passkey: MULTICA_PASSKEY_RP_ID must not be a public suffix")
		}
		rpID = o
	}
	origin := u.Scheme + "://" + host
	if port != "" && !(u.Scheme == "https" && port == "443") && !(u.Scheme == "http" && port == "80") {
		origin += ":" + port
	}
	return Config{RPID: rpID, Origin: origin}, nil
}

// WebAuthn builds the go-webauthn relying party. Passkeys are discoverable
// credentials with required user verification; attestation is not requested
// because a private deployment has no authenticator allowlist to check it
// against.
func (c Config) WebAuthn() (*webauthn.WebAuthn, error) {
	return webauthn.New(&webauthn.Config{
		RPID:          c.RPID,
		RPDisplayName: "Multica",
		RPOrigins:     []string{c.Origin},
		AuthenticatorSelection: protocol.AuthenticatorSelection{
			ResidentKey:      protocol.ResidentKeyRequirementRequired,
			UserVerification: protocol.VerificationRequired,
		},
		Timeouts: webauthn.TimeoutsConfig{
			Login:        webauthn.TimeoutConfig{Enforce: true, Timeout: ChallengeTTL, TimeoutUVD: ChallengeTTL},
			Registration: webauthn.TimeoutConfig{Enforce: true, Timeout: ChallengeTTL, TimeoutUVD: ChallengeTTL},
		},
	})
}

// User adapts a Multica user and their stored passkeys to webauthn.User.
// The user handle is the user's UUID, so a discoverable assertion names the
// account it belongs to without any server-side lookup table.
type User struct {
	Row         db.User
	Credentials []webauthn.Credential
}

func (u User) WebAuthnID() []byte                         { return u.Row.ID.Bytes[:] }
func (u User) WebAuthnName() string                       { return u.Row.Email }
func (u User) WebAuthnDisplayName() string                { return u.Row.Name }
func (u User) WebAuthnCredentials() []webauthn.Credential { return u.Credentials }

// LoadUser reads a user's stored passkeys into a webauthn.User.
func LoadUser(ctx context.Context, q *db.Queries, user db.User) (User, error) {
	rows, err := q.ListWebauthnCredentialsByUser(ctx, user.ID)
	if err != nil {
		return User{}, err
	}
	creds := make([]webauthn.Credential, 0, len(rows))
	for _, row := range rows {
		var c webauthn.Credential
		if err := json.Unmarshal(row.Credential, &c); err != nil {
			return User{}, fmt.Errorf("decode passkey %x: %w", row.ID.Bytes, err)
		}
		creds = append(creds, c)
	}
	return User{Row: user, Credentials: creds}, nil
}

// ErrUserNotEnrollable means the account already has a passkey or belongs to
// a workspace, so a workspace admin may not hand out a link that would let
// someone else register a passkey on it.
var ErrUserNotEnrollable = errors.New("account already exists; only the host operator can issue a recovery link")

// IssueEnrollmentToken mints a one-time enrollment secret for userID and
// stores only its hash. createdBy is the admin who asked for it, or invalid
// when the host operator did.
func IssueEnrollmentToken(ctx context.Context, q *db.Queries, userID, createdBy pgtype.UUID) (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	token := base64.RawURLEncoding.EncodeToString(raw[:])
	expiresAt := time.Now().Add(EnrollmentTTL)
	if _, err := q.CreatePasskeyEnrollmentToken(ctx, db.CreatePasskeyEnrollmentTokenParams{
		UserID:    userID,
		TokenHash: auth.HashToken(token),
		CreatedBy: createdBy,
		ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true},
	}); err != nil {
		return "", time.Time{}, err
	}
	return token, expiresAt, nil
}

// EnrollmentURL is the link a person opens to register a passkey. The token
// travels in the query string of the app's own login page, so no new route
// is introduced.
func EnrollmentURL(appOrigin, token string) string {
	return strings.TrimRight(appOrigin, "/") + "/login?enroll=" + url.QueryEscape(token)
}

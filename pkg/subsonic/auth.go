package subsonic

import (
	"crypto/md5"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
)

// Authenticate verifies Subsonic credentials from query/form params.
//
// Supported schemes (all official clients use one of these):
//   - token/salt:  ?u=user&t=<md5(pass+salt)>&s=<salt>
//   - plain:      ?u=user&p=password
//   - hex:        ?u=user&p=enc:<hex-of-password>
func Authenticate(r *http.Request, wantUser, wantPass string) bool {
	_ = r.ParseForm() // best effort; query alone is enough for GET
	q := r.Form
	gotUser := firstNonEmpty(q.Get("u"), q.Get("username"))
	if gotUser == "" || subtle.ConstantTimeCompare([]byte(gotUser), []byte(wantUser)) != 1 {
		return false
	}
	if t, s := q.Get("t"), q.Get("s"); t != "" && s != "" {
		sum := md5.Sum([]byte(wantPass + s))
		want := hex.EncodeToString(sum[:])
		// Clients send lowercase hex; compare case-insensitively.
		t = strings.ToLower(t)
		if len(t) != len(want) {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(t), []byte(want)) == 1
	}
	if p := q.Get("p"); p != "" {
		if strings.HasPrefix(p, "enc:") {
			raw, err := hex.DecodeString(strings.TrimPrefix(p, "enc:"))
			if err != nil {
				return false
			}
			if len(raw) != len(wantPass) {
				return false
			}
			return subtle.ConstantTimeCompare(raw, []byte(wantPass)) == 1
		}
		if len(p) != len(wantPass) {
			return false
		}
		return subtle.ConstantTimeCompare([]byte(p), []byte(wantPass)) == 1
	}
	return false
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

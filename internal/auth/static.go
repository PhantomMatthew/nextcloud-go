package auth

import (
	"context"
	"crypto/subtle"
)

type StaticVerifier struct {
	UID         string
	Password    string
	DisplayName string
}

func NewStaticVerifier(uid, password, displayName string) *StaticVerifier {
	return &StaticVerifier{UID: uid, Password: password, DisplayName: displayName}
}

func (v *StaticVerifier) Verify(_ context.Context, user, password string) (*Principal, error) {
	if user == "" || password == "" {
		return nil, ErrNoCredentials
	}
	uOK := subtle.ConstantTimeCompare([]byte(user), []byte(v.UID)) == 1
	pOK := subtle.ConstantTimeCompare([]byte(password), []byte(v.Password)) == 1
	if !uOK || !pOK {
		return nil, ErrInvalidCredentials
	}
	return &Principal{UID: v.UID, DisplayName: v.DisplayName, Enabled: true, AuthMethod: AuthMethodBasic}, nil
}

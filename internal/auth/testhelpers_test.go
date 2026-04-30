package auth

import "encoding/base64"

func encStd(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

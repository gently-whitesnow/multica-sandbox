package repo

import "strings"

// rotatesToken reports a GitLab token rotation: .../personal_access_tokens/<id|self>/rotate
// and .../access_tokens/<id|self>/rotate of groups, projects and service accounts. GitLab
// revokes the rotated token and returns its successor, so rotating the token the relay
// holds would hand the attempt a working host credential and revoke the password file.
// A token can rotate itself by its own id, which the relay does not know, so every
// rotation is refused. The decoded path is compared whatever the method, case, trailing
// slash or format suffix; creating tokens is left to the token's scope.
func rotatesToken(decodedPath string) bool {
	segments := strings.FieldsFunc(strings.ToLower(decodedPath), func(r rune) bool { return r == '/' })
	n := len(segments)
	if n < 3 {
		return false
	}
	last := strings.TrimSuffix(strings.TrimSuffix(segments[n-1], ".json"), ".xml")
	return last == "rotate" && (segments[n-3] == "personal_access_tokens" || segments[n-3] == "access_tokens")
}

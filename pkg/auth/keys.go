package auth

import (
	"crypto/sha256"
	"encoding/hex"
)

func userKey(id string) string            { return "agentlink:v2:user:" + id }
func usernameKey(name string) string      { return "agentlink:v2:username:" + name }
func userTeamsKey(id string) string       { return userKey(id) + ":teams" }
func teamKey(id string) string            { return "agentlink:v2:team:" + id }
func teamMembersKey(id string) string     { return teamKey(id) + ":members" }
func teamProjectsKey(id string) string    { return teamKey(id) + ":projects" }
func webSessionKey(hash string) string    { return "agentlink:v2:web_session:" + hash }
func deviceSessionKey(hash string) string { return "agentlink:v2:device_session:" + hash }
func userWebSessionsKey(id string) string { return userKey(id) + ":web_sessions" }
func userDeviceSessionsKey(id string) string {
	return userKey(id) + ":device_sessions"
}
func deviceKey(id string) string          { return "agentlink:v2:device:" + id }
func userDevicesKey(userID string) string { return userKey(userID) + ":devices" }
func deviceCredentialKey(sessionHash string) string {
	return "agentlink:v2:device_credential:" + sessionHash
}
func loginFailUserKey(normalized string) string {
	return "agentlink:v2:login_fail:user:" + sha256Hex(normalized)
}
func loginFailIPKey(ip string) string {
	return "agentlink:v2:login_fail:ip:" + sha256Hex(ip)
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func SecretHash(secret string) string {
	return sha256Hex(secret)
}

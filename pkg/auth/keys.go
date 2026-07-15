package auth

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

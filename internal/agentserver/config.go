/*
Copyright 2026 Date Huang.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package agentserver

import "time"

// DefaultSessionTTL bounds how long a session token minted at
// registration (RegisterResponse.SessionToken) is accepted on GET
// .../next, when Config.SessionTTL is zero. It is generous compared to
// DefaultTokenTTL in internal/bootserver: a session has to outlive the
// live environment's entire inspect-then-deploy window, not just the
// net boot itself, and a large image transfer over a slow link can take
// a while.
const DefaultSessionTTL = 6 * time.Hour

// Config configures a Server.
type Config struct {
	// Addr is the address the HTTP server listens on, for example
	// ":8091".
	Addr string
	// StoreRoot is the store's local filesystem root, mounted read-only
	// into the manager container out-of-band - the same volume and the
	// same requirement internal/controller.SeederConfig.StoreRoot
	// documents (see config/seeder's README): this server reads
	// contents/<hash>/torrent.info directly to build the .torrent bytes
	// a DeployPlan hands the agent. Leaving it empty is not an error by
	// itself; it just means GET .../next can never build a plan for an
	// Image with a content partition; every reconcile-worthy poll then
	// answers ActionWait forever, which is easy to mistake for "still
	// resolving" instead of "misconfigured" - operators enabling
	// AGENT_SERVER_ADDR should also set AGENT_STORE_ROOT.
	StoreRoot string
	// TrackerURL is inserted as the "announce" field of every .torrent
	// this server builds (see internal/store.BuildTorrentFile) -
	// typically the same value as SEEDER_TRACKER_URL, since the agent's
	// leecher and the seeder join the same swarm.
	TrackerURL string
	// SessionTTL bounds how long a minted session token is accepted on
	// GET .../next. Zero means DefaultSessionTTL.
	SessionTTL time.Duration
}

// sessionTTL returns c.SessionTTL, or DefaultSessionTTL when unset.
func (c Config) sessionTTL() time.Duration {
	if c.SessionTTL <= 0 {
		return DefaultSessionTTL
	}
	return c.SessionTTL
}

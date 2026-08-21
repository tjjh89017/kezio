/*
Copyright 2026.

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
// registration (RegisterResponse.SessionToken) is accepted on
// GET/POST /agent/next, when Config.SessionTTL is zero. It is generous
// compared to internal/bootserver.DefaultTokenTTL: a session has to
// outlive the live environment's entire session, not just the net boot
// itself.
const DefaultSessionTTL = 6 * time.Hour

// DefaultPollInterval is the NextResponse.PollIntervalSeconds value
// used when Config.PollInterval is zero.
const DefaultPollInterval = 15 * time.Second

// Config configures a Server.
type Config struct {
	// Addr is the address the HTTP server listens on, for example
	// ":8091".
	Addr string
	// SessionTTL bounds how long a minted session token is accepted on
	// GET/POST /agent/next. Zero means DefaultSessionTTL.
	SessionTTL time.Duration
	// PollInterval is the poll interval reported to the agent in every
	// NextResponse. Zero means DefaultPollInterval.
	PollInterval time.Duration
}

// sessionTTL returns c.SessionTTL, or DefaultSessionTTL when unset.
func (c Config) sessionTTL() time.Duration {
	if c.SessionTTL <= 0 {
		return DefaultSessionTTL
	}
	return c.SessionTTL
}

// pollInterval returns c.PollInterval, or DefaultPollInterval when
// unset.
func (c Config) pollInterval() time.Duration {
	if c.PollInterval <= 0 {
		return DefaultPollInterval
	}
	return c.PollInterval
}

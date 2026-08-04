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

package kezioctl

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Environment variables ResolveServerURL and ResolveToken fall back to
// when their matching flag is empty.
const (
	// ServerEnvVar names the image service base URL when --server is
	// not given.
	ServerEnvVar = "KEZIOCTL_SERVER"
	// TokenEnvVar names the bearer token when --token is not given.
	TokenEnvVar = "KEZIOCTL_TOKEN"
	// TokenFileEnvVar names a file holding the bearer token when neither
	// --token nor --token-file is given.
	TokenFileEnvVar = "KEZIOCTL_TOKEN_FILE"
)

// ResolveServerURL returns the image service base URL: the --server flag
// if set, otherwise the KEZIOCTL_SERVER environment variable. It errors
// when neither is set, since `kezioctl image upload` cannot proceed
// without knowing where to stream the file.
func ResolveServerURL(flagValue string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	if env := os.Getenv(ServerEnvVar); env != "" {
		return env, nil
	}
	return "", fmt.Errorf("no image service URL given: set --server or %s", ServerEnvVar)
}

// ResolveToken returns the bearer token to authenticate an upload with,
// checking sources in order of most to least explicit so a value given
// closer to the command line always wins over one set further away in
// the environment:
//
//  1. --token (the flag value passed in directly)
//  2. the KEZIOCTL_TOKEN environment variable
//  3. --token-file (a file path passed in directly), its content trimmed
//     of exactly one trailing newline
//  4. the KEZIOCTL_TOKEN_FILE environment variable, read the same way
//
// It errors when none of these yield a non-empty token, since an upload
// without a bearer token can only fail authentication server-side.
func ResolveToken(tokenFlag, tokenFileFlag string) (string, error) {
	if tokenFlag != "" {
		return tokenFlag, nil
	}
	if env := os.Getenv(TokenEnvVar); env != "" {
		return env, nil
	}
	if tokenFileFlag != "" {
		return readTokenFile(tokenFileFlag)
	}
	if env := os.Getenv(TokenFileEnvVar); env != "" {
		return readTokenFile(env)
	}
	return "", fmt.Errorf("no upload token given: set --token, --token-file, %s, or %s", TokenEnvVar, TokenFileEnvVar)
}

func readTokenFile(path string) (string, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is an operator-supplied CLI flag/env value, not external input
	if err != nil {
		return "", fmt.Errorf("read token file %s: %w", path, err)
	}
	token := strings.TrimSuffix(string(data), "\n")
	token = strings.TrimSuffix(token, "\r")
	if token == "" {
		return "", errors.New("token file " + path + " is empty")
	}
	return token, nil
}

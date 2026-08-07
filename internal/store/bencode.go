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

package store

import (
	"bytes"
	"fmt"
)

// This file hand-rolls the minimal bencoding (BEP3) this package needs:
// writing a fixed-shape info dict and wrapping it into a .torrent file.
// A generic bencode dependency would still need this file's dict-key
// ordering logic to produce a canonical, hash-stable info dict, so one
// was not added.

// bencodeString writes a bencoded byte string: "<len>:<bytes>". BEP3
// strings are byte strings, not necessarily UTF-8 text, so this takes
// []byte rather than string.
func bencodeString(buf *bytes.Buffer, s []byte) {
	fmt.Fprintf(buf, "%d:", len(s))
	buf.Write(s)
}

// bencodeInt writes a bencoded integer: "i<n>e".
func bencodeInt(buf *bytes.Buffer, n int64) {
	fmt.Fprintf(buf, "i%de", n)
}

// bencodeExtentFile appends one BEP3 multi-file "files" list entry:
// {"length": <n>, "path": [<name>]}. Dict keys are written in the required
// sorted order ("length" < "path").
func bencodeExtentFile(buf *bytes.Buffer, name string, length uint64) {
	buf.WriteByte('d')
	bencodeString(buf, []byte("length"))
	bencodeInt(buf, int64(length)) //nolint:gosec // extent lengths fit int64 (partition sizes are far below 2^63)
	bencodeString(buf, []byte("path"))
	buf.WriteByte('l')
	bencodeString(buf, []byte(name))
	buf.WriteByte('e')
	buf.WriteByte('e')
}

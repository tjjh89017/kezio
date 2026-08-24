#!/usr/bin/env python3
"""Extract the "announce" value from a .torrent file's bencoded bytes.

store.BuildTorrentFile (internal/store/torrent.go) always writes
"announce" as the dict's first key, in strict bencode string form
"<len>:<bytes>". This is not a general bencode parser - it locates that
one fixed key and reads its length-prefixed value, which is all a
producer with a known, fixed key layout requires.
"""
import sys

if len(sys.argv) != 2:
    sys.exit(f"usage: {sys.argv[0]} <torrent-file-path>")

data = open(sys.argv[1], "rb").read()
marker = b"8:announce"
idx = data.find(marker)
if idx == -1:
    sys.exit("no announce key found in served .torrent bytes")
pos = idx + len(marker)
colon = data.index(b":", pos)
length = int(data[pos:colon])
start = colon + 1
sys.stdout.write(data[start:start + length].decode("utf-8"))

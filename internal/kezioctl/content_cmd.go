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

package kezioctl

import (
	"fmt"

	"github.com/spf13/cobra"
)

// newContentCmd builds the `kezioctl content` command group: local,
// cluster-independent tooling around PartitionContent's content-addressed
// naming. Unlike the image verbs, these commands never talk to the
// cluster or the image service - flags is accepted for symmetry with
// newImageCmd but currently unused.
func newContentCmd(_ *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "content",
		Short: "Inspect locally-ingested partition content",
	}
	cmd.AddCommand(newContentHashCmd())
	return cmd
}

func newContentHashCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hash <content-dir>",
		Short: "Print the PartitionContent name a local content directory will hash to",
		Long: `content hash reads the torrent.info a local
"partclone.<fs> -c -T -o <content-dir> <partition-file>" run left in
<content-dir> and prints the PartitionContent object name ("pc-<info
hash>") ingest will compute for the same bytes.

An Image's spec (including every slot's contentRef) is immutable, so a
layout file that wants ingest to populate a PartitionContent for a slot
must declare that slot's contentRef before the Image is created. Content
addressing makes the name fully determined by the partition's bytes,
independent of where or when it is hashed - so running partclone locally
against the same source partition and hashing its output here yields the
exact name the ingest Job will compute once it processes that partition
server-side.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := ContentDirObjectName(args[0])
			if err != nil {
				return err
			}
			_, _ = fmt.Fprintln(cmd.OutOrStdout(), name)
			return nil
		},
	}
	return cmd
}

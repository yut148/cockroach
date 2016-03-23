// Copyright 2016 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License. See the AUTHORS file
// for names of contributors.
//
// Author: Ben Darnell

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"

	"github.com/cockroachdb/cockroach/keys"
	"github.com/cockroachdb/cockroach/roachpb"
	"github.com/cockroachdb/cockroach/storage"
	"github.com/cockroachdb/cockroach/storage/engine"
	"github.com/cockroachdb/cockroach/util/stop"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/gogo/protobuf/proto"
	"github.com/spf13/cobra"
)

var debugKeysCmd = &cobra.Command{
	Use:   "keys [directory]",
	Short: "dump all the keys in a store",
	Long: `
Pretty-prints all keys in a store.
`,
	RunE: runDebugKeys,
}

func openStore(cmd *cobra.Command, dir string, stopper *stop.Stopper) (engine.Engine, error) {
	initCacheSize()

	db := engine.NewRocksDB(roachpb.Attributes{}, dir,
		cliContext.CacheSize, cliContext.MemtableBudget, 0, stopper)
	if err := db.Open(); err != nil {
		return nil, err
	}
	return db, nil
}

func printKey(kv engine.MVCCKeyValue) (bool, error) {
	fmt.Printf("%q\n", kv.Key)

	return false, nil
}

func printKeyValue(kv engine.MVCCKeyValue) (bool, error) {
	if kv.Key.Timestamp != roachpb.ZeroTimestamp {
		fmt.Printf("%s %q: ", kv.Key.Timestamp, kv.Key.Key)
	} else {
		fmt.Printf("%q: ", kv.Key.Key)
	}
	for _, decoder := range []func(kv engine.MVCCKeyValue) (string, error){tryRaftLogEntry, tryRangeDescriptor, tryMeta, trySequence, tryTxn} {
		out, err := decoder(kv)
		if err != nil {
			continue
		}
		fmt.Println(out)
		return false, nil
	}
	// No better idea, just print raw bytes and hope that folks use `less -S`.
	fmt.Printf("%q\n\n", kv.Value)
	return false, nil
}

func runDebugKeys(cmd *cobra.Command, args []string) error {
	stopper := stop.NewStopper()
	defer stopper.Stop()

	if len(args) != 1 {
		return errors.New("one argument is required")
	}

	db, err := openStore(cmd, args[0], stopper)
	if err != nil {
		return err
	}

	d := cliContext.debug

	from := engine.NilKey
	to := engine.MVCCKeyMax
	if d.raw {
		if len(d.startKey) > 0 {
			from = engine.MakeMVCCMetadataKey(roachpb.Key(d.startKey))
		}
		if len(d.endKey) > 0 {
			to = engine.MakeMVCCMetadataKey(roachpb.Key(d.endKey))
		}
	} else {
		if len(d.startKey) > 0 {
			startKey, err := keys.UglyPrint(d.startKey)
			if err != nil {
				return err
			}
			from = engine.MakeMVCCMetadataKey(startKey)
		}
		if len(d.endKey) > 0 {
			endKey, err := keys.UglyPrint(d.endKey)
			if err != nil {
				return err
			}
			to = engine.MakeMVCCMetadataKey(endKey)
		}
	}

	printer := printKey
	if d.values {
		printer = printKeyValue
	}

	if err := db.Iterate(from, to, printer); err != nil {
		return err
	}

	return nil
}

var debugRangeDescriptorsCmd = &cobra.Command{
	Use:   "range-descriptors [directory]",
	Short: "print all range descriptors in a store",
	Long: `
Prints all range descriptors in a store with a history of changes.
`,
	RunE: runDebugRangeDescriptors,
}

func descStr(desc roachpb.RangeDescriptor) string {
	return fmt.Sprintf("[%s, %s)\n\tRaw:%s\n",
		desc.StartKey, desc.EndKey, &desc)
}

func tryMeta(kv engine.MVCCKeyValue) (string, error) {
	if !bytes.HasPrefix(kv.Key.Key, keys.Meta1Prefix) && !bytes.HasPrefix(kv.Key.Key, keys.Meta2Prefix) {
		return "", errors.New("not a meta key")
	}
	value := roachpb.Value{
		Timestamp: kv.Key.Timestamp,
		RawBytes:  kv.Value,
	}
	var desc roachpb.RangeDescriptor
	if err := value.GetProto(&desc); err != nil {
		return "", err
	}
	return descStr(desc), nil
}

func maybeUnmarshalInline(v []byte, dest proto.Message) error {
	var meta engine.MVCCMetadata
	if err := meta.Unmarshal(v); err != nil {
		return err
	}
	value := roachpb.Value{
		RawBytes: meta.RawBytes,
	}
	return value.GetProto(dest)
}

func tryTxn(kv engine.MVCCKeyValue) (string, error) {
	var txn roachpb.Transaction
	if err := maybeUnmarshalInline(kv.Value, &txn); err != nil {
		return "", err
	}
	return txn.String() + "\n", nil
}

func trySequence(kv engine.MVCCKeyValue) (string, error) {
	if kv.Key.Timestamp != roachpb.ZeroTimestamp {
		return "", errors.New("not a sequence cache key")
	}
	_, _, _, err := keys.DecodeSequenceCacheKey(kv.Key.Key, nil)
	if err != nil {
		return "", err
	}
	var dest roachpb.SequenceCacheEntry
	if err := maybeUnmarshalInline(kv.Value, &dest); err != nil {
		return "", err
	}
	return fmt.Sprintf("ts=%s, key=%q\n", dest.Timestamp, dest.Key), nil
}

func tryRangeDescriptor(kv engine.MVCCKeyValue) (string, error) {
	_, suffix, _, err := keys.DecodeRangeKey(kv.Key.Key)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(suffix, keys.LocalRangeDescriptorSuffix) {
		return "", fmt.Errorf("wrong suffix: %s", suffix)
	}
	value := roachpb.Value{
		RawBytes: kv.Value,
	}
	var desc roachpb.RangeDescriptor
	if err := value.GetProto(&desc); err != nil {
		return "", err
	}
	return descStr(desc), nil
}

func printRangeDescriptor(kv engine.MVCCKeyValue) (bool, error) {
	if out, err := tryRangeDescriptor(kv); err != nil {
		fmt.Printf("%s %q: invalid value: %v", kv.Key.Timestamp, kv.Key.Key, err)
	} else {
		fmt.Printf("%s %q: %s\n", kv.Key.Timestamp, kv.Key.Key, out)
	}
	return false, nil
}

func runDebugRangeDescriptors(cmd *cobra.Command, args []string) error {
	stopper := stop.NewStopper()
	defer stopper.Stop()

	if len(args) != 1 {
		return errors.New("one argument is required")
	}

	db, err := openStore(cmd, args[0], stopper)
	if err != nil {
		return err
	}

	start := engine.MakeMVCCMetadataKey(keys.LocalRangePrefix)
	end := engine.MakeMVCCMetadataKey(keys.LocalRangeMax)

	if err := db.Iterate(start, end, printRangeDescriptor); err != nil {
		return err
	}
	return nil
}

var debugRaftLogCmd = &cobra.Command{
	Use:   "raft-log [directory] [range id]",
	Short: "print the raft log for a range",
	Long: `
Prints all log entries in a store for the given range.
`,
	RunE: runDebugRaftLog,
}

func tryRaftLogEntry(kv engine.MVCCKeyValue) (string, error) {
	var ent raftpb.Entry
	if err := maybeUnmarshalInline(kv.Value, &ent); err != nil {
		return "", err
	}
	if ent.Type == raftpb.EntryNormal {
		if len(ent.Data) > 0 {
			_, cmdData := storage.DecodeRaftCommand(ent.Data)
			var cmd roachpb.RaftCommand
			if err := cmd.Unmarshal(cmdData); err != nil {
				return "", err
			}
			ent.Data = nil
			return fmt.Sprintf("%s by %v\n%s\n%s\n", &ent, cmd.OriginReplica, &cmd.Cmd, &cmd), nil
		}
		return fmt.Sprintf("%s: EMPTY\n", &ent), nil
	} else if ent.Type == raftpb.EntryConfChange {
		var cc raftpb.ConfChange
		if err := cc.Unmarshal(ent.Data); err != nil {
			return "", err
		}
		var ctx storage.ConfChangeContext
		if err := ctx.Unmarshal(cc.Context); err != nil {
			return "", err
		}
		var cmd roachpb.RaftCommand
		if err := cmd.Unmarshal(ctx.Payload); err != nil {
			return "", err
		}
		ent.Data = nil
		return fmt.Sprintf("%s\n%s\n", &ent, &cmd), nil
	}
	return "", fmt.Errorf("Unknown log entry type: %s\n", &ent)
}

func printRaftLogEntry(kv engine.MVCCKeyValue) (bool, error) {
	if out, err := tryRaftLogEntry(kv); err != nil {
		fmt.Printf("%q: %v\n\n", kv.Key.Key, err)
	} else {
		fmt.Printf("%q: %s\n", kv.Key.Key, out)
	}
	return false, nil
}

func runDebugRaftLog(cmd *cobra.Command, args []string) error {
	stopper := stop.NewStopper()
	defer stopper.Stop()

	if len(args) != 2 {
		return errors.New("required arguments: dir range_id")
	}

	db, err := openStore(cmd, args[0], stopper)
	if err != nil {
		return err
	}

	rangeIDInt, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil {
		return err
	}
	rangeID := roachpb.RangeID(rangeIDInt)

	start := engine.MakeMVCCMetadataKey(keys.RaftLogPrefix(rangeID))
	end := engine.MakeMVCCMetadataKey(keys.RaftLogPrefix(rangeID).PrefixEnd())

	if err := db.Iterate(start, end, printRaftLogEntry); err != nil {
		return err
	}
	return nil
}

var debugCmds = []*cobra.Command{
	debugKeysCmd,
	debugRangeDescriptorsCmd,
	debugRaftLogCmd,
	kvCmd,
	rangeCmd,
}

var debugCmd = &cobra.Command{
	Use:   "debug [command]",
	Short: "debugging commands",
	Long: `Various commands for debugging.

These commands are useful for extracting data from the data files of a
process that has failed and cannot restart.
`,
	Run: func(cmd *cobra.Command, args []string) {
		mustUsage(cmd)
	},
}

func init() {
	debugCmd.AddCommand(debugCmds...)
}

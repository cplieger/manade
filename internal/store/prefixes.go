package store

// Every table's prefix byte. A prefix is unique across the schema and names
// the table in the change feed, so a prefix is never reused for another table.
const (
	prefixMeta       byte = 0x01
	prefixSecrets    byte = 0x10
	prefixHosts      byte = 0x20
	prefixTombstones byte = 0x21
	prefixScaleSets  byte = 0x30
	prefixJournal    byte = 0x40
	prefixRuns       byte = 0x50
	prefixRunBlobs   byte = 0x51
	prefixHostEvents byte = 0x52
	prefixProbes     byte = 0x53
	prefixRollups    byte = 0x60
)

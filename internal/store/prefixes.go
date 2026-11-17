package store

// Every table's prefix byte. A prefix is unique across the schema and names
// the table in the change feed, so a prefix is never reused for another table.
const (
	prefixMeta      byte = 0x01
	prefixScaleSets byte = 0x02
)

package main

// Minimal Yjs update-v1 codec for the "single-writer appends binary items to a
// root Y.Array" pattern. Enough to (a) probe whether notes.mail.ru's sync
// channel carries document content to peers, and (b) later carry tunnel bytes.
//
// Update-v1 layout (lib0 varint encoding):
//
//	varUint numClients
//	repeat numClients:
//	  varUint numStructs
//	  varUint client
//	  varUint clock              // clock of the first struct in the run
//	  repeat numStructs:          // each struct is an Item here
//	    u8 info                   // low5 = content ref; 0x80 origin; 0x40 right; 0x20 parentSub
//	    [if origin]   ID(client,clock)
//	    [if right]    ID(client,clock)
//	    [if !origin && !right] parentInfo + (rootName | parentID) [+ parentSub]
//	    <content by ref>
//	  <delete set>                // varUint numClients (0 = empty)
//
// Content refs: 0 GC, 1 Deleted, 2 JSON, 3 Binary, 4 String, 8 Any.

const (
	yContentDeleted = 1
	yContentBinary  = 3
	yContentString  = 4
	yContentAny     = 8

	yInfoOrigin    = 0x80
	yInfoRight     = 0x40
	yInfoParentSub = 0x20
	yInfoRefMask   = 0x1f
)

// encodeBinaryAppends builds an update that appends each chunk as a ContentBinary
// item to the root array `root`, authored by `client`, with the first item at
// `startClock`. The very first item ever (startClock==0) carries the parent
// (root name); later items chain off the previous item via their left origin.
func encodeBinaryAppends(client, startClock uint64, root string, chunks [][]byte) []byte {
	e := newVarEncoder()
	e.writeVarUint(1)                     // one client block
	e.writeVarUint(uint64(len(chunks)))   // struct count
	e.writeVarUint(client)                // client id
	e.writeVarUint(startClock)            // first clock
	for k, ch := range chunks {
		clock := startClock + uint64(k)
		hasOrigin := clock != 0
		info := byte(yContentBinary)
		if hasOrigin {
			info |= yInfoOrigin
		}
		e.buf = append(e.buf, info)
		if hasOrigin {
			e.writeVarUint(client)
			e.writeVarUint(clock - 1) // left origin = previous item
		} else {
			// origin==null && right==null → write the parent (a root type)
			e.writeVarUint(1) // parentInfo: true = root type addressed by name
			e.writeVarString(root)
			// array parent → parentSub is null → nothing
		}
		e.writeVarUint8Array(ch) // ContentBinary payload (raw bytes)
	}
	e.writeVarUint(0) // empty delete set
	return e.bytes()
}

// encodeDeleteUpdate builds an update with NO new structs, only a delete set
// that tombstones [clock, clock+length) for one client. The server (gc:true)
// garbage-collects the deleted content, keeping the doc small.
func encodeDeleteUpdate(client, clock, length uint64) []byte {
	e := newVarEncoder()
	e.writeVarUint(0) // 0 clients in the structs section
	// delete set:
	e.writeVarUint(1) // 1 client
	e.writeVarUint(client)
	e.writeVarUint(1) // 1 range
	e.writeVarUint(clock)
	e.writeVarUint(length)
	return e.bytes()
}

// yjsItem is one decoded struct we care about.
type yjsItem struct {
	client uint64
	clock  uint64
	ref    byte
	data   []byte // for Binary/String content
}

// decodeYjsUpdate parses an update-v1 blob and returns the content-bearing
// items. It is tolerant: on an unparseable struct it stops early and returns
// what it has plus ok=false, so a probe can still show partial results.
func decodeYjsUpdate(update []byte) (items []yjsItem, ok bool) {
	d := newVarDecoder(update)
	numClients, err := d.readVarUint()
	if err != nil {
		return items, false
	}
	for c := uint64(0); c < numClients; c++ {
		numStructs, err := d.readVarUint()
		if err != nil {
			return items, false
		}
		client, err := d.readVarUint()
		if err != nil {
			return items, false
		}
		clock, err := d.readVarUint()
		if err != nil {
			return items, false
		}
		for s := uint64(0); s < numStructs; s++ {
			info, err := d.readByte()
			if err != nil {
				return items, false
			}
			ref := info & yInfoRefMask
			hasOrigin := info&yInfoOrigin != 0
			hasRight := info&yInfoRight != 0
			hasParentSub := info&yInfoParentSub != 0

			if hasOrigin {
				if _, err = d.readVarUint(); err != nil { // origin client
					return items, false
				}
				if _, err = d.readVarUint(); err != nil { // origin clock
					return items, false
				}
			}
			if hasRight {
				if _, err = d.readVarUint(); err != nil {
					return items, false
				}
				if _, err = d.readVarUint(); err != nil {
					return items, false
				}
			}
			if !hasOrigin && !hasRight {
				parentInfo, err := d.readVarUint()
				if err != nil {
					return items, false
				}
				if parentInfo == 1 {
					if _, err = d.readVarString(); err != nil { // root name
						return items, false
					}
				} else {
					if _, err = d.readVarUint(); err != nil { // parent item id client
						return items, false
					}
					if _, err = d.readVarUint(); err != nil { // parent item id clock
						return items, false
					}
				}
				if hasParentSub {
					if _, err = d.readVarString(); err != nil {
						return items, false
					}
				}
			}

			length := uint64(1)
			switch ref {
			case yContentBinary:
				b, err := d.readVarUint8Array()
				if err != nil {
					return items, false
				}
				items = append(items, yjsItem{client: client, clock: clock, ref: ref, data: b})
				length = 1
			case yContentString:
				str, err := d.readVarString()
				if err != nil {
					return items, false
				}
				items = append(items, yjsItem{client: client, clock: clock, ref: ref, data: []byte(str)})
				length = uint64(len([]rune(str)))
				if length == 0 {
					length = 1
				}
			case 0: // GC
				n, err := d.readVarUint()
				if err != nil {
					return items, false
				}
				length = n
			case yContentDeleted:
				n, err := d.readVarUint()
				if err != nil {
					return items, false
				}
				length = n
			default:
				// content type we don't parse (JSON/Any/Embed/Format/Type/Doc)
				return items, false
			}
			clock += length
		}
	}
	return items, true
}

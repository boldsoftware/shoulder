package main

import (
	"crypto/rand"
	mathrand "math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
)

// Session ids are two short words, so they are easy to read off one tab
// and type into another.
var (
	idAdjectives = []string{
		"amber", "brave", "brisk", "calm", "clever", "cosmic", "crisp", "dapper",
		"eager", "fancy", "fuzzy", "gentle", "giddy", "golden", "happy", "hazel",
		"jolly", "keen", "lively", "lucky", "mellow", "merry", "misty", "nimble",
		"noble", "plucky", "proud", "quick", "quiet", "rapid", "rosy", "rusty",
		"shiny", "silver", "sleepy", "snappy", "spry", "steady", "sunny", "swift",
		"tidy", "witty", "zesty",
	}
	idNouns = []string{
		"badger", "beaver", "bison", "comet", "crane", "falcon", "ferret", "finch",
		"fox", "gecko", "heron", "ibis", "koala", "lemur", "lynx", "marmot",
		"moose", "newt", "otter", "owl", "panda", "pika", "puffin", "quail",
		"raven", "robin", "salmon", "seal", "sparrow", "stoat", "tapir", "toucan",
		"trout", "turtle", "walrus", "wombat", "wren", "yak", "zebra",
	}
)

// newID picks an id not already used by a session in dir.
func newID(dir string) string {
	for i := 0; ; i++ {
		id := idAdjectives[mathrand.IntN(len(idAdjectives))] + "-" + idNouns[mathrand.IntN(len(idNouns))]
		if i >= 20 {
			id += "-" + strconv.Itoa(i)
		}
		if _, err := os.Stat(filepath.Join(dir, id)); os.IsNotExist(err) {
			return id
		}
	}
}

// newCode makes an access code: at least 128 bits from the system's
// random source, as text that goes in a URL. A code is pasted into an
// agent's command line, never typed, so its length costs nothing, and it
// is too long to guess at any rate.
func newCode() string {
	return rand.Text()
}

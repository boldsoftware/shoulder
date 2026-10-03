package main

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
		id := idAdjectives[rand.IntN(len(idAdjectives))] + "-" + idNouns[rand.IntN(len(idNouns))]
		if i >= 20 {
			id += "-" + strconv.Itoa(i)
		}
		if _, err := os.Stat(filepath.Join(dir, id)); os.IsNotExist(err) {
			return id
		}
	}
}

// codeWords are for access codes, magic-wormhole style: a number and two
// words, like "7-river-amber". Short, easy to read aloud and retype.
var codeWords = strings.Fields(`
	acid acorn actor adult agent alarm album alley alpha amber anchor angle ankle apple april apron
	arena armor arrow atlas attic audio autumn avenue badge bagel baker bamboo banjo barley basil basket
	beach beacon beard beetle bench berry bicycle bingo birch biscuit blanket blossom border bottle bounce bracket
	breeze brick bridge bronze brush bubble bucket buffalo bundle butter cabin cactus camera canal candle canoe
	canyon carbon cargo carpet castle cello cereal chalk channel cherry chess chimney cider cinema circus citrus
	clover cobalt cocoa coffee comet copper coral cotton cradle crater crayon cricket crystal cupcake cyclone dancer
	delta denim desert diesel dinner dolphin domino donkey dragon drift drum eagle echo eclipse elbow ember
	engine falcon feather fennel ferry fiddle field figure filter flame flannel fossil fountain galaxy garden garlic
	gazelle ginger glacier globe gopher granite gravel guitar hammer harbor harvest helmet hermit hollow honey horizon
	iceberg igloo indigo island ivory jacket jaguar jasmine jelly jigsaw jungle kettle kiwi koala ladder lagoon
	lantern laser lemon lettuce linen lobster locket lotus lumber magnet mango maple marble meadow melon meteor
	mirror mitten monkey mosaic muffin mustard napkin nectar needle noodle nutmeg oasis ocean olive onion orbit
	orchid otter oyster paddle pajama panda paper parrot pasta peanut pebble pencil pepper piano pickle pigeon
	pillow pilot planet plaza pocket polka pony popcorn potato prism puzzle quartz quiver rabbit radar radio
	raisin ranch raven record ribbon river rocket saddle saffron salmon sandal satin sauce scarf season shadow
	shovel signal silver sketch sled slipper socket spider sponge spruce squash statue summit sunset sweater tango
`)

// newCode makes an access code. Use of one is rate limited on network
// shares (see server.agent), which is what lets it be this short.
func newCode() string {
	return strconv.Itoa(1+rand.IntN(99)) + "-" + codeWords[rand.IntN(len(codeWords))] + "-" + codeWords[rand.IntN(len(codeWords))]
}

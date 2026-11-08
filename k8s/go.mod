// patch.go (go run .) patches a Kubernetes checkout and copies tabernetes/ into it,
// where it is built. _overlay/ and tabernetes/ only compile inside that checkout.
module tabernetes-overlay

go 1.27.3

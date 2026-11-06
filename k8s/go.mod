// patch.go (go run .) patches a Kubernetes checkout and copies kwasm/ into it,
// where it is built. _overlay/ and kwasm/ only compile inside that checkout.
module kwasm-overlay

go 1.26

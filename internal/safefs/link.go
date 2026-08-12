package safefs

import "os"

// SingleLink reports whether info is known not to be hard-linked. Sources that
// do not expose a link count through os.FileInfo — Windows, and virtual
// filesystems such as embed.FS — cannot answer this and report true, because a
// blanket rejection would refuse every entry instead of the dangerous ones.
// Those paths re-verify with SingleLinkFile once a descriptor is held.
func SingleLink(info os.FileInfo) bool {
	count, known := linkCount(info)
	return !known || count == 1
}

func singleLink(info os.FileInfo) bool { return SingleLink(info) }

// SingleLinkFile answers the same question from an open descriptor, which is
// authoritative on every supported platform.
func SingleLinkFile(f *os.File) (bool, error) { return singleLinkFile(f) }

// HasFileIdentity reports whether os.SameFile can compare info meaningfully.
// Virtual filesystems return FileInfo values that SameFile always rejects, so
// identity re-checks must be gated on this.
func HasFileIdentity(info os.FileInfo) bool { return hasFileIdentity(info) }

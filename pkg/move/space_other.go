//go:build !unix

package move

// availableBytes has no portable implementation off unix. Returning 0 makes the
// preflight skip the space check rather than invent a number, which is the same
// thing it does when the source's disk size cannot be parsed.
func availableBytes(string) (int64, error) { return 0, nil }

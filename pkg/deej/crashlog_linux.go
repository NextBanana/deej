package deej

// redirectStderrToFile is a no-op outside windows, where stderr is already
// connected to something the user can see
func redirectStderrToFile(path string) error {
	return nil
}

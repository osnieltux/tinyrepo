package main

type PackageDeb struct {
	Package     string
	Version     string
	InstalledSize     string
	Maintainer  string
	Arch	    int8
	Size        string
	Filename    string
	FilenameUrl string	// TODO: field to store the base URL of the package although it is not the standard
	MD5sum      string
	SHA256      string
	Pre_Depends string
	Breaks      string
	Description string
	Tag         string
	Section     string
	Priority    string
	Homepage    string
	Depends     string
	idsDepends  []int	// TODO: use in the future
	id          int     // TODO: use in the future
}
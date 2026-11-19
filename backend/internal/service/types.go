package service

const PlatformMailbox = "mailbox"

type Group struct {
	ID       int64
	Platform string
}

type APIKey struct {
	ID    int64
	Key   string
	Group Group
}

module github.com/akunzai/glov/windows

go 1.27

require (
	github.com/Microsoft/go-winio v0.6.4
	github.com/ProtonMail/go-crypto v1.5.2
	github.com/akunzai/glov/diskimage v0.0.0
	github.com/akunzai/glov/guest v0.0.0
	github.com/akunzai/glov/skills v0.0.0
	github.com/modelcontextprotocol/go-sdk v1.8.0
	golang.org/x/sys v0.49.0
)

require (
	github.com/cloudflare/circl v1.6.3 // indirect
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/crypto v0.52.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.23.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/akunzai/glov/guest => ../guest

replace github.com/akunzai/glov/skills => ../skills

replace github.com/akunzai/glov/diskimage => ../diskimage

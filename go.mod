module github.com/nizartuanku/topolight

go 1.24

replace golang.org/x/crypto => github.com/golang/crypto v0.40.0

replace golang.org/x/sys => github.com/golang/sys v0.34.0

replace golang.org/x/term => github.com/golang/term v0.33.0

require (
	github.com/nizartuanku/hexward-core v0.1.0
	golang.org/x/crypto v0.40.0
)

require golang.org/x/sys v0.34.0 // indirect

.PHONY: templ bench

templ:
	go tool templ generate ./internal/cloud/dashboard/...

bench:
	go test -bench=. -benchmem -run='^$$' ./internal/store/

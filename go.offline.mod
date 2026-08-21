module github.com/yellowman/wavecontrol

go 1.21

require (
	github.com/go-chi/chi/v5 v5.0.11
	github.com/go-chi/cors v1.2.1
	github.com/golang-jwt/jwt/v5 v5.2.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/websocket v1.5.1
	github.com/lib/pq v1.10.9
	golang.org/x/crypto v0.17.0
	golang.org/x/sys v0.15.0
)

replace github.com/go-chi/chi/v5 => ./verification_ui_reports/offline_deps/chi

replace github.com/go-chi/cors => ./verification_ui_reports/offline_deps/cors

replace github.com/golang-jwt/jwt/v5 => ./verification_ui_reports/offline_deps/jwt

replace github.com/google/uuid => ./verification_ui_reports/offline_deps/uuid

replace github.com/gorilla/websocket => ./verification_ui_reports/offline_deps/websocket

replace github.com/lib/pq => ./verification_ui_reports/offline_deps/pq

replace golang.org/x/crypto => ./verification_ui_reports/offline_deps/crypto

replace golang.org/x/sys => ./verification_ui_reports/offline_deps/sys

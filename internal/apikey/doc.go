// Package apikey holds the API-key format shared by the API's bootstrap
// path and the sorotrail command. The implementations live with the code
// that authenticates against them; this package exists so both sides
// import one definition of the format rather than two copies.
package apikey

# Change Log

## v0.2.0 (2026-08-26)
- Added `ServerWrappers` which are middlewares that wrap the mux directly, applying on all handlers AND those requests return 404 and 405. These wrappers are specifically used to handle CORS preflight and/or panic recovery.

#### Breaking changes
- Added `Tier` function to the User interface
- Changed the signature of the authentication middleware to have access to the cookies and headers

## v0.1.0 (2026-08-26)
- initial release
# Olla Emporium

A small, self-contained website for Jade's pottery, built with Go's standard library. The HTML, styles, JavaScript and photographs are embedded in the Go executable.

The entry point is `main.go`; the application and its Go tests live in `internal/site/`, with embedded website files in `internal/site/web/`. JavaScript tests live in `test/`, and the original Etsy about text is in `docs/`.

## Run locally

With Go 1.20 or newer installed, run:

```sh
go run .
```

Open <http://localhost:8080>. The server listens on port 8080 by default; set the `PORT` environment variable to use a different port.

To build a standalone executable, run `go build .`. The default demo works without external services or dependencies.

## Configuration

The app reads an optional `.env` file from its current working directory at startup. Edit the local `.env`, or copy `.env.example` to `.env` if it is missing. The file is ignored by Git; keep it private because it may contain passwords and Etsy credentials. Restart the app after editing it. If the file is absent, the app uses its normal defaults. Environment variables already set by the operating system take precedence, even when empty.

Use one `KEY=value` assignment per line. Blank lines and lines beginning with `#` are ignored; `#` elsewhere is part of the value. Values may be unquoted, enclosed in single quotes for literal text, or enclosed in double quotes with Go-style escapes. For example, use single quotes around a Windows path containing backslashes. Inline comments, variable expansion and multiline values are not supported. An invalid `.env` stops startup with the line number rather than silently ignoring the problem.

## Active Etsy listings

The carousel defaults to `ETSY_LISTINGS_MODE=mock`: it displays clearly labeled example pieces using the local photographs, without connecting to Etsy. You may set `ETSY_ACCESS_TOKEN` to a fake development value in this mode; it is ignored and never sent anywhere. The links go to the [Olla Emporium Etsy shop](https://www.etsy.com/shop/OllaEmporiumFR).

To display real Etsy listings, configure these environment variables on the server and restart it:

| Variable | Value |
| --- | --- |
| `ETSY_LISTINGS_MODE` | `etsy` |
| `ETSY_SHOP_ID` | The shop's **numeric** Etsy shop ID, not its name or URL |
| `ETSY_KEYSTRING` | Your Etsy application's keystring |
| `ETSY_SHARED_SECRET` | Your Etsy application's shared secret |
| `ETSY_ACCESS_TOKEN` | Optional OAuth bearer token; leave unset for these public endpoints |

Create an Etsy app and obtain its credentials from Etsy. Keep all credentials private; they stay on the Go server and are not sent to browsers. The server sends them as `x-api-key: keystring:secret`. Etsy's public active-listings and listing-image endpoints do not require OAuth. If `ETSY_ACCESS_TOKEN` is set in live mode, it is sent to Etsy as a bearer token; use a real token, since a fake one may be rejected. There is no OAuth login or refresh flow in this site. Mock mode is a placeholder for development, not real OAuth authentication.

The storefront shows active shop listings through Etsy's `findAllActiveListingsByShop` endpoint. The server paginates that endpoint and aggregates the results through the existing `/api/featured-listings` route and response format. Etsy caps the pagination offset at 12,000; if a shop requires a higher offset, the server reports an error instead of returning a partial catalogue. The active-listings endpoint does not document an `includes=Images` parameter, so the server fetches images separately through `/listings/batch`, using `includes=Images` and up to 100 listing IDs per request.

Live mode caches successful results for five minutes per server process. Paginated listing requests and image batches are subject to Etsy API access, rate limits and availability. If Etsy is unavailable or the configuration is incomplete, the page displays a message and a direct shop link instead of presenting demo items as real listings. The browser loads listing images from Etsy when live mode is enabled.

If the listings API returns a 502, check the server log for `Etsy listings refresh failed`. It identifies the failed request and Etsy's HTTP status when available, or reports a network, parsing or validation failure. These diagnostics do not include credentials, listing data, request URLs or Etsy response bodies. An optional access token, if configured, is sent to Etsy and may be rejected if expired; it is not needed for these public requests.

The home page randomly selects its main photographs from website-only products and the real Etsy listings returned by the server on each page load. When available, each photograph links to its product page. If no usable listings or images are available, the original photographs remain as non-product fallbacks; demo Etsy items are not treated as real products. Etsy images are selected from the retrieved active listings.

Clicking an Etsy listing opens an on-site detail page. The Buy on Etsy button opens the listing on Etsy, where the buyer selects any options and adds it to their basket. Etsy does not document a public API or URL to add a listing directly to a visitor's basket. Details are available for retrieved listings while they remain active, subject to the cache.

## Website-only products

Set `ADMIN_PASSWORD` to a strong, unique password on the server and restart it. Use the **Log in** link at the bottom of the site to create a website-only product with a title, description, price and JPEG or PNG photo (up to 5 MiB, 4096 pixels per side and 16 megapixels). Admin is disabled when the password is unset. Login sessions expire after eight hours or a server restart. Secure HTTPS-only admin access is enabled by default; for local HTTP development only, explicitly set `ADMIN_COOKIE_SECURE=false`. Never use that setting on a public server.

Products and photos are stored in `./data` by default. Set `PRODUCT_DATA_DIR` to a persistent, writable directory to change this location, and back it up regularly. Store this directory outside any publicly served static directory. The storage and login throttling are designed for a single server process, not multiple instances sharing a directory. When products exist, their carousel appears before Etsy's; otherwise its empty state appears after Etsy's.

For deployment, serve the admin over HTTPS and keep `ADMIN_COOKIE_SECURE` unset or set it to `true`, so the session cookie is restricted to HTTPS. In this mode, the admin accepts direct TLS or a loopback reverse proxy sending exactly one `X-Forwarded-Proto: https` header. The proxy must terminate TLS, overwrite client-supplied forwarding headers, and protect the backend port from external access. Login throttling uses the direct peer IP, so visitors behind the same proxy share a limit; apply per-client limits at the proxy if needed. Do not expose the admin password or product storage publicly. Website-only products have an enquiry link, not an online checkout; no payment or inventory management is implemented.

## Tests

```sh
go test ./...
node --test test/frontend_test.cjs
```

The frontend tests use Node.js and its built-in test runner.

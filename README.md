# Silo OMDb Metadata

A [Silo](https://github.com/Silo-Server/silo-server) metadata plugin for films TMDB does not list, such as fan films and fan edits. It looks them up on [OMDb](https://www.omdbapi.com/), which is built from IMDb's data, and supplies the title, year, plot, genres, runtime, cast, IMDb rating and poster.

## How it spends requests

OMDb's free key allows 1,000 requests a day, and Silo asks every metadata source on every lookup. The plugin only calls OMDb when TMDB cannot answer:

| Lookup | OMDb request? |
|---|---|
| IMDb ID and no TMDB ID (a `{imdb-tt…}` folder tag, or the IMDb ID field in the Match dialog) | Yes, once a day per film |
| IMDb ID beside a TMDB ID | No: TMDB already knows the film |
| Title only | No, unless **Search by title** is on |
| Refresh of a film matched through OMDb | Yes, once a day per film |
| Refresh of a film matched through TMDB | No: Silo never asks this plugin |

The first title-only search after the plugin starts looks up one film to prove the API key works, so **Check Connection** catches a wrong key. Answers are kept for 24 hours, and the plugin stops at its **Daily request limit** (900 by default) until midnight UTC.

## Setup

1. Get a free API key at https://www.omdbapi.com/apikey.aspx.
2. Install the plugin in Silo (**Admin → Plugins → Upload**) with the `plugin-linux-amd64` binary from a release.
3. Paste the key under **OMDb**, save, and run **Check Connection**.
4. Keep OMDb below TMDB in each movie library's metadata sources. It declares a low default priority, so new libraries already place it last.
5. For a film TMDB cannot find, add `{imdb-tt…}` to its folder name, or enter the IMDb ID in the Match dialog.

## Build

```sh
make build      # ./plugin for this machine
make build-all  # dist/plugin-linux-{amd64,arm64}
make test
```

Pushing a `v*` tag, or running the Release workflow by hand, builds the binaries and attaches them to a GitHub release.

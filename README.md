# httpreplay

[![Coverage](./.badges/coverage.svg)](#)

`httpreplay` is an **efficient, resumable** command-line interface (CLI) tool for replaying HTTP requests from a tape file. It simulates traffic while enforcing configurable **QPS** (queries per second) and **concurrency** limits.

> [!NOTE]
> Except for the documentation, every line of code is human-crafted. **No AI slop included**.

## Features

- Replay HTTP requests from a specified tape file (in a `curl`-like format).
- Configurable **QPS** and **concurrency** limits.
- **Resumable Operations**: Automatically tracks and saves the last processed request position using a position file, allowing for safe, interruption-tolerant restarts (e.g., after `Ctrl+C` or system failure).
- **Client-Side Load Balancing**: Prevents replayed traffic from being pinned to a single backend behind a host.
- Dry-run mode to preview requests without sending them.
- Logs failed requests to a dedicated failure tape file for easy retry or analysis.
- Detailed progress logging with real-time success rate and QPS metrics.
- **Graceful Shutdown**: Handles `SIGINT` and `SIGTERM` to ensure all state and logs are properly saved.

---

## Installation

### Option 1: Download Pre-Built Binary

Visit the [Latest Release](https://github.com/roy2220/httpreplay/releases/latest) page.

### Option 2: Build from Source

Requires Go toolchain 1.26+.

```bash
GOBIN=${PWD} CGO_ENABLED=0 go install github.com/roy2220/httpreplay@latest
```

---

## Usage

Run `httpreplay` with the required tape file and optional flags:

```bash
httpreplay TAPE-FILE [-n NUM] [-q QPS] [-c CONCURRENCY] [-t SECONDS] [-f] [-d] [--clb-hot-conns-per-host NUM] [--clb-max-conn-age SECONDS] [--clb-max-conn-age-jitter FRACTION] [--delivery-semantics SEMANTICS] [--failure-tape-buffer-size BYTES] [--sync-to-disk-interval SECONDS]
```

### Basic Arguments

| Argument | Description | Default |
| :--- | :--- | :--- |
| **TAPE-FILE** (required) | Path to the tape file containing HTTP requests. | |
| **-n NUM** | Stop after `NUM` requests, no limit if less than 0. | `-1` |
| **-q QPS** | Maximum QPS, no limit if less than 1. | `1` |
| **-c CONCURRENCY** | Maximum concurrency, no limit if less than 1. | `1` |
| **-t SECONDS** | Request timeout in seconds (float), no timeout if less than or equal to 0.0. | `10.0` |
| **-f** | Follow HTTP redirects. | `false` |
| **-d** | **Dry-run mode**: Preview requests without sending them. | `false` |

> **Note**: At least one of QPS or concurrency must be limited (i.e., $\ge 1$).

### Advanced Arguments

| Argument | Description | Default |
| :--- | :--- | :--- |
| **--clb-hot-conns-per-host NUM** | Minimum number of independent connections actively used per host.<br>See [Client-Side Load Balancing](#client-side-load-balancing) | `10` |
| **--clb-max-conn-age SECONDS** | Soft upper bound on how long a connection may be reused, in seconds (float); in-flight requests are allowed to finish.<br>See [Client-Side Load Balancing](#client-side-load-balancing) | `300.0` |
| **--clb-max-conn-age-jitter FRACTION** | Jitter fraction in (0.0, 1.0] applied to the maximum connection age; the effective age is drawn uniformly from `[(1-jitter)*max, (1+jitter)*max]`.<br>See [Client-Side Load Balancing](#client-side-load-balancing) | `0.5` |
| **--delivery-semantics SEMANTICS** | Delivery semantics used when resuming after a crash: `at-least-once` or `at-most-once`.<br>See [Delivery Semantics](#delivery-semantics) | `at-least-once` |
| **--failure-tape-buffer-size BYTES** | Buffer size of the failure tape file, in bytes. Values too small will be raised. | `16777216` |
| **--sync-to-disk-interval SECONDS** | Interval in seconds (float) for syncing the failure tape and position file to disk. Values too small will be raised. | `0.5` |

-----

## Tape File Format

The tape file contains **one HTTP request per line**, in a format similar to `curl` arguments. `httpreplay` uses a robust parser to interpret the line into an HTTP request with method, URL, headers, and optional body.

**Key Flags Supported:**

- `-X`, `--request`: HTTP method (e.g., `POST`, `PUT`). Default is `GET`.
- `-H`, `--header`: Header (e.g., `"Content-Type: application/json"`).
- `-d`, `--data`: Request body/data.

### Example Tape File (`requests.txt`)

```
https://example.com/api/status  # Shell-style comments are allowed
https://example.com/api/post -X POST -H "Content-Type: application/json" -d '{"key":"value"}'
https://example.com/api/auth -X GET -H "Authorization: Bearer token"
```

-----

## Quick Start & Examples

1. **Standard replay:** Sets a **QPS limit of 10** and a **concurrency limit of 5**.

    ```
    httpreplay requests.txt -q 10 -c 5
    ```

2. **Maximum QPS:** Limits only **concurrency to 100** for maximum possible throughput (limited only by the target server and network).

    ```
    httpreplay requests.txt -q 0 -c 100
    ```

3. **Limit by QPS only:** Limits only **QPS to 50**, allowing unlimited concurrent requests.

    ```
    httpreplay requests.txt -q 50 -c 0
    ```

4. **Replay only the next 100 requests:** Useful for batch processing a large tape incrementally.

    ```
    httpreplay requests.txt -n 100 -q 10 -c 5
    ```

5. **Dry-run:** Previews requests to the console without actually sending them.

    ```
    httpreplay requests.txt -d
    ```

6. **Follow redirects:** Replays requests, following any HTTP redirects (e.g., `301` or `302`).

    ```
    httpreplay requests.txt -f
    ```

> **Tip**: You can stop the process safely with **`Ctrl+C`**. It will save its progress and can resume when run again.

### Output Files

`httpreplay` creates companion files next to your `TAPE-FILE` to manage state:

- **Failure Tape File** (`TAPE-FILE.httpreplay-failure`): Stores the raw lines of any failed requests for later analysis or retry. *Synced to disk periodically (default: every 500ms; see `--sync-to-disk-interval`).*
- **Position File** (`TAPE-FILE.httpreplay-pos`): Tracks the last processed request index for resuming. What counts as processed depends on the delivery semantics. *Uses memory-mapping for atomic, resilient updates.*
- **Dry-run Position File** (`TAPE-FILE.httpreplay-pos.dry-run`): A separate position file is used when running in dry-run mode (`-d`) to prevent overwriting the main position file.

-----

## Logging

- **Progress Logs**: Displayed every second, showing the tape position, real-time QPS, current concurrency, skipped/successful/failed counts, and success rate.
- **Debug Logs**: Enable verbose logging for parsed HTTP requests by setting the environment variable `DEBUG=1`.
- **Errors/Warnings**: Logged with `[WARN]` or `[FATAL]` for file issues, parsing failures, etc.

**Example Progress Log:**

```
[INFO] current progress: tapePosition=100 qps=50 concurrency=5 skipped=0 successful=95 failed=5 successRate=0.95
```

-----

## Client-Side Load Balancing

Keep-alive connections can pin replayed traffic to one or a few of the backends behind a single host. Two knobs spread it out:

- **Hot connections per host** (`--clb-hot-conns-per-host`): requests fan out across at least this many independent connections, so even a QPS-1 replay reaches multiple backends.
- **Maximum connection age** (`--clb-max-conn-age`, `--clb-max-conn-age-jitter`): a connection stops taking new requests once it reaches this age (in-flight ones finish), and new requests re-pick the target backend via a new connection. Jitter draws each effective age uniformly from `[(1-jitter)*max, (1+jitter)*max]`, so connections don't all retire at once.

-----

## Delivery Semantics

By default, `httpreplay` resumes with **at-least-once** semantics: a request counts as processed once completed, and after an abrupt crash (`SIGKILL`, out of memory) everything after the last completed-in-order request is resent — including requests whose responses had already arrived but were stuck behind a slower one. Pass `--delivery-semantics at-most-once` to count requests as processed once sent instead, so a resumed run never duplicates requests but skips the ones that were in flight at crash time. A graceful stop (`Ctrl+C` / `SIGTERM`) always lets in-flight requests finish under either setting.

-----

## Troubleshooting

- **Invalid Tape Format**: Ensure each line strictly adheres to the `curl`-like syntax. Check logs for parsing errors.
- **Resuming Issues**: If the position file (`*.httpreplay-pos`) is manually edited or corrupted, delete it to restart the replay from the beginning.
- **Performance**: If your failure rate is high or QPS is low, adjust the `-q` (QPS limit) and `-c` (concurrency limit) flags to balance the load on the target system.

-----

## License

MIT License. See `LICENSE` for details.

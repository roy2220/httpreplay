# HTTP Replay Tool

This is a Go-based command-line tool for replaying HTTP requests from a tap file, with configurable QPS (queries per second) and concurrency limits. It processes HTTP requests, logs successes and failures, and saves failed requests to a separate file for further analysis.

## Features
- Reads HTTP requests from a specified tap file.
- **Tracks and saves the position in the tap file for resuming.**
- Logs failed requests to a `.httpreplay-failure` file.
- Supports rate limiting (QPS) and concurrency control.
- Configurable HTTP request timeout.
- Displays real-time statistics (QPS, concurrency, success rate, etc.).
- Graceful shutdown on SIGINT/SIGTERM signals.

## Installation
```bash
go install github.com/roy2220/httpreplay
```

## Usage
Run the tool with the following command:
```bash
./httpreplay TAP-FILE [options]
```

### Arguments
- **TAP-FILE** (required): Path to the tap file containing HTTP requests. Each line should be a valid HTTP request in a format parseable by `shlex` (e.g., `curl`-like syntax: `URL -X METHOD -H "Header: Value"`).
- **-q QPS**: Queries per second limit. Set to `< 1` for no limit (default: 1).
- **-c CONCURRENCY**: Concurrent request limit. Set to `< 1` for no limit (default: 1).
- **-t TIMEOUT**: HTTP request timeout in seconds. Set to `< 1` for no timeout (default: 10).

**Note**: At least one of QPS or concurrency must be limited (i.e., both cannot be `< 1`).

### Example
To replay requests from `requests.tap` with 10 QPS, 5 concurrent requests, and a 5-second timeout:
```bash
./httpreplay requests.tap -q 10 -c 5 -t 5
```

### Tap File Format
The tap file should contain one HTTP request per line, formatted similarly to `curl` commands. Example:
```
http://example.com/api -X GET -H 'Authorization: Bearer token'
http://example.com/post -X POST -H 'Content-Type: application/json' -d '{"hello":"world"}'
```

### Output
- **Logs**: Real-time statistics are logged every second, showing:
  - Current position in the tap file.
  - QPS (requests per second).
  - Current concurrency.
  - Successful and failed request counts.
  - Success rate.
  Example log:
  ```
  [INFO] position: 100, qps: 8, concurrency: 5, successful: 95, failed: 5, success rate: 0.95
  ```
- **Failure File**: Failed requests are appended to `TAP-FILE.httpreplay-failure`.
- **Position File**: The current position in the tap file is saved to `TAP-FILE.httpreplay-pos` every 200ms, allowing resumption after interruption.

## Debugging
Enable debug mode to log detailed HTTP request information:
```bash
DEBUG=1 ./httpreplay requests.tap
```

## Stopping the Tool
- Press `Ctrl+C` (SIGINT) or send a `SIGTERM` to stop the tool gracefully.
- The tool will:
  - Save the current tap file position.
  - Flush failed requests to the failure file.
  - Close all files and exit.

## Notes
- The tool resumes from the last saved position in `TAP-FILE.httpreplay-pos` if available.
- Failed requests are those that result in errors or non-2xx HTTP status codes.
- Ensure the tap file is accessible and properly formatted to avoid parsing errors.
- Large tap files are supported, with a 16MB buffer for reading and writing.

## Troubleshooting
- **"failed to parse http request"**: Check the tap file for invalid request formats.
- **"failed to open tap file"**: Verify the file path and permissions.
- **High failure rate**: Check network connectivity, server availability, or increase the timeout with `-t`.

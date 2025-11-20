# logwat

Watch new lines appended to log files in a directory (optionally recursive) with a simple terminal UI.

![Screenshot](screenshots/logwat.png)

## Quick start
1) Build (requires Go 1.22+):

```
go build -o logwat .
```

2) Run:

```
./logwat <directory> [pattern] [--recursive]

# Examples
./logwat /var/log
./logwat C:/ProgramData/Microsoft/IntuneManagementExtension/Logs *.log --recursive
```

Notes
- pattern is a glob matched against file names (e.g., `*.log`, `*error*.txt`).
- Use `--recursive` or `-r` to include subdirectories.
- Paths render with backslashes on Windows for readability.

## Demo
Try it with the included sample logs:

```
./logwat ./demo-logs *.txt
```

Append lines to any file in `demo-logs` to see them appear live.

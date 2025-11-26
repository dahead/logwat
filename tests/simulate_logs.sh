#!/bin/bash

# Log simulation script - generates random log entries to /tmp/*.log files

LOG_DIR="/tmp"
LOG_FILES=("app.log" "system.log" "error.log" "access.log" "vpn.audit.log", "windows_update_kb123456789_error.log", "intune_ref14234handle.iwin.log")
INTERVAL=2  # seconds between log entries

# Array of log prefixes to make it more realistic
PREFIXES=(
  "[INFO]"
  "[DEBUG]"
  "[WARN]"
  "[ERROR]"
  "[TRACE]"
  "INFO -"
  "DEBUG -"
  "ERROR -"
  "FATAL -"
)

# Function to generate random string of variable length
generate_random_text() {
  local length=$((RANDOM % 1000 + 24))  # 24-1024 characters
  head -c "$length" /dev/urandom | tr -dc 'A-Za-z0-9 _.,;:()[]{}' | fold -w 80 | head -1
}

# Function to generate a realistic log line
generate_log_line() {
  local prefix="${PREFIXES[$((RANDOM % ${#PREFIXES[@]}))]}"
  local timestamp=$(date '+%Y-%m-%d %H:%M:%S')
  local thread_id=$((RANDOM % 16 + 1))
  local text=$(generate_random_text)
  
  # Different formats for different log types
  case $((RANDOM % 4)) in
    0)
      echo "$timestamp [$thread_id] $prefix $text"
      ;;
    1)
      echo "$timestamp - $prefix - Thread-$thread_id - $text"
      ;;
    2)
      echo "[$prefix] [$((RANDOM % 5000 + 1000))ms] $text"
      ;;
    3)
      echo "$timestamp | $prefix | PID:$$ | $text"
      ;;
  esac
}

# Trap Ctrl+C for graceful shutdown
trap 'echo "Stopping log simulation..."; exit 0' SIGINT SIGTERM

echo "Starting log simulation in $LOG_DIR/"
echo "Press Ctrl+C to stop"
sleep 1

# Main loop
while true; do
  # Pick a random log file
  log_file="$LOG_DIR/${LOG_FILES[$((RANDOM % ${#LOG_FILES[@]}))]}"
  
  # Generate 1-3 log lines per iteration
  num_lines=$((RANDOM % 3 + 1))
  for ((i = 0; i < num_lines; i++)); do
    generate_log_line >> "$log_file"
  done
  
  ms=$((RANDOM % 100))
  sleep "0.$(printf '%03d' "$ms")"
done

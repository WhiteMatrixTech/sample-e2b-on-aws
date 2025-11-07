#!/bin/bash
set -xe

# === [INIT EFS] ===
${init_script}

# === [START CLIENT] ===
${main_script}

echo "[user_data] 所有启动脚本执行完毕"

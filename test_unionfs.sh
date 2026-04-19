#!/bin/bash
# Automated test suite for Mini-UnionFS. Adapted from Appendix B of the spec:
# the Go binary is launched in the background so the script can drive the
# mount point immediately.

FUSE_BINARY="./mini_unionfs"
TEST_DIR="./unionfs_test_env"
LOWER_DIR="$TEST_DIR/lower"
UPPER_DIR="$TEST_DIR/upper"
MOUNT_DIR="$TEST_DIR/mnt"

GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

pass=0
fail=0
report() {
    if [ "$1" = "ok" ]; then
        echo -e "${GREEN}PASSED${NC}"
        pass=$((pass + 1))
    else
        echo -e "${RED}FAILED${NC}"
        fail=$((fail + 1))
    fi
}

echo "Starting Mini-UnionFS Test Suite..."

# ---- Setup -----------------------------------------------------------------
rm -rf "$TEST_DIR"
mkdir -p "$LOWER_DIR" "$UPPER_DIR" "$MOUNT_DIR"
echo "base_only_content" > "$LOWER_DIR/base.txt"
echo "to_be_deleted"     > "$LOWER_DIR/delete_me.txt"

# Launch FUSE in the background.
"$FUSE_BINARY" "$LOWER_DIR" "$UPPER_DIR" "$MOUNT_DIR" &
FUSE_PID=$!

# Wait until the mount is live (up to ~5s).
for _ in 1 2 3 4 5 6 7 8 9 10; do
    if mount | grep -q "$MOUNT_DIR" || [ -f "$MOUNT_DIR/base.txt" ]; then
        break
    fi
    sleep 0.5
done

cleanup() {
    fusermount -u "$MOUNT_DIR" 2>/dev/null || umount "$MOUNT_DIR" 2>/dev/null
    wait "$FUSE_PID" 2>/dev/null
    rm -rf "$TEST_DIR"
}
trap cleanup EXIT

# ---- Test 1: Visibility ----------------------------------------------------
echo -n "Test 1: Layer Visibility... "
if grep -q "base_only_content" "$MOUNT_DIR/base.txt" 2>/dev/null; then
    report ok
else
    report fail
fi

# ---- Test 2: Copy-on-Write -------------------------------------------------
echo -n "Test 2: Copy-on-Write... "
echo "modified_content" >> "$MOUNT_DIR/base.txt" 2>/dev/null
if [ "$(grep -c "modified_content" "$MOUNT_DIR/base.txt" 2>/dev/null)" -eq 1 ] \
   && [ "$(grep -c "modified_content" "$UPPER_DIR/base.txt" 2>/dev/null)" -eq 1 ] \
   && [ "$(grep -c "modified_content" "$LOWER_DIR/base.txt" 2>/dev/null)" -eq 0 ]; then
    report ok
else
    report fail
fi

# ---- Test 3: Whiteout ------------------------------------------------------
echo -n "Test 3: Whiteout mechanism... "
rm "$MOUNT_DIR/delete_me.txt" 2>/dev/null
if [ ! -f "$MOUNT_DIR/delete_me.txt" ] \
   && [ -f "$LOWER_DIR/delete_me.txt" ] \
   && [ -f "$UPPER_DIR/.wh.delete_me.txt" ]; then
    report ok
else
    report fail
fi

# ---- Test 4: Merged readdir ------------------------------------------------
echo -n "Test 4: Merged readdir... "
echo "upper_only" > "$UPPER_DIR/upper_only.txt"
listing=$(ls "$MOUNT_DIR" 2>/dev/null | sort | tr '\n' ' ')
# delete_me.txt should be hidden; whiteouts should never appear.
if echo "$listing" | grep -q "base.txt" \
   && echo "$listing" | grep -q "upper_only.txt" \
   && ! echo "$listing" | grep -q "delete_me.txt" \
   && ! echo "$listing" | grep -q ".wh."; then
    report ok
else
    report fail
fi

# ---- Test 5: Create then read ----------------------------------------------
echo -n "Test 5: create/write/read new file... "
echo "fresh" > "$MOUNT_DIR/new.txt"
if [ "$(cat "$MOUNT_DIR/new.txt" 2>/dev/null)" = "fresh" ] \
   && [ -f "$UPPER_DIR/new.txt" ]; then
    report ok
else
    report fail
fi

# ---- Test 6: mkdir / rmdir -------------------------------------------------
echo -n "Test 6: mkdir/rmdir in upper... "
mkdir "$MOUNT_DIR/sub" 2>/dev/null
touch "$MOUNT_DIR/sub/x" 2>/dev/null
rm "$MOUNT_DIR/sub/x" 2>/dev/null
if rmdir "$MOUNT_DIR/sub" 2>/dev/null && [ ! -d "$UPPER_DIR/sub" ]; then
    report ok
else
    report fail
fi

# ---- Summary ---------------------------------------------------------------
echo "------------------------------------------------"
echo "Passed: $pass    Failed: $fail"
echo "Test Suite Completed."
[ "$fail" -eq 0 ]

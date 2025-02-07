#!/usr/bin/env bats
# vim: set syntax=sh:

load helpers

function setup() {
	setup_test
}

function teardown() {
	cleanup_test
}

@test "default config should be empty" {
	setup_crio
	output=$(env -i "$CRIO_BINARY_PATH" -c "" -d "" config | sed 's/#.*//g' | sed 's/\[.*//g' | tr -d '\n')
	[[ "$output" == "" ]]
}

@test "config dir should succeed" {
	# given
	setup_crio

	printf "[crio.runtime]\npids_limit = 1234\n" > "$CRIO_CONFIG_DIR"/00-default
	printf "[crio.runtime]\npids_limit = 5678\n" > "$CRIO_CONFIG_DIR"/01-overwrite

	# when
	start_crio_no_setup
	output=$("${CRIO_STATUS_BINARY_PATH}" --socket="${CRIO_SOCKET}" config)

	# then
	[[ "$output" == *"pids_limit = 5678"* ]]
}

@test "config dir should fail with invalid option" {
	# given
	printf '[crio.runtime]\nlog_level = "info"\n' > "$CRIO_CONFIG"
	printf '[crio.runtime]\nlog_level = "wrong-level"\n' > "$CRIO_CONFIG_DIR"/00-default

	# when
	run "$CRIO_BINARY_PATH" -c "$CRIO_CONFIG" -d "$CRIO_CONFIG_DIR"

	# then
	[ "$status" -ne 0 ]
	[[ "$output" == *"not a valid logrus"*"wrong-level"* ]]
}

@test "choose different default runtime should succeed" {
	# when
	unset CONTAINER_RUNTIMES
	RES=$("$CRIO_BINARY_PATH" -c "$TESTDATA"/50-crun-default.conf -d "" config 2>&1)

	# then
	[[ "$RES" == *"default_runtime = \"crun\""* ]]
	[[ "$RES" == *"crio.runtime.runtimes.runc"* ]]
	[[ "$RES" == *"crio.runtime.runtimes.crun"* ]]
}

@test "runc not existing when default_runtime changed should succeed" {
	# when
	unset CONTAINER_RUNTIMES
	cat << EOF > "$TESTDIR"/50-runc-new-path.conf
[crio.runtime]
default_runtime = "crun"
[crio.runtime.runtimes.runc]
runtime_path = "/not/there"
[crio.runtime.runtimes.crun]
runtime_path="/usr/bin/crun"
EOF
	RES=$("$CRIO_BINARY_PATH" -c "$TESTDIR"/50-runc-new-path.conf -d "" config 2>&1)

	# then
	[[ "$RES" == *"default_runtime = \"crun\""* ]]
	[[ "$RES" == *"crio.runtime.runtimes.runc"* ]]
	[[ "$RES" == *"crio.runtime.runtimes.crun"* ]]
}

@test "retain default runtime should succeed" {
	# when
	RES=$("$CRIO_BINARY_PATH" -c "$TESTDATA"/50-crun.conf -d "" config 2>&1)

	# then
	[[ "$RES" != *"default_runtime = \"crun\""* ]]
	[[ "$RES" == *"crio.runtime.runtimes.runc"* ]]
	[[ "$RES" == *"crio.runtime.runtimes.crun"* ]]
}

@test "monitor fields should be translated" {
	if [[ "$RUNTIME_TYPE" == "vm" ]]; then
		skip "not applicable to vm runtime type"
	fi
	# when
	RES=$("$CRIO_BINARY_PATH" --conmon-cgroup="pod" --conmon="/bin/true" -c "" -d "" config 2>&1)

	# then
	[[ "$RES" == *"monitor_cgroup = \"pod\""* ]]
	[[ "$RES" == *"monitor_path = \"/bin/true\""* ]]
}

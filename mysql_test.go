package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExportData_Mysql(t *testing.T) {
	dummyPath := "/tmp/test_mysql_dump_dir"
	os.MkdirAll(dummyPath, 0755)
	defer os.RemoveAll(dummyPath)

	t.Run("explicit DBName", func(t *testing.T) {
		_, executedCommands, teardown := setupTest()
		defer teardown()

		cfg := SyncConfig{
			DBType:    "mysql",
			DBName:    "testdb",
			SourceURI: "mysql://user:pass@localhost:3306/testdb",
			Threads:   "4",
		}

		err := exportData(cfg, dummyPath)
		require.NoError(t, err)

		require.NotEmpty(t, *executedCommands)
		assert.Contains(t, (*executedCommands)[0], "mydumper")
		assert.Contains(t, (*executedCommands)[0], "--database testdb")
	})

	t.Run("DBName from URI", func(t *testing.T) {
		_, executedCommands, teardown := setupTest()
		defer teardown()

		cfg := SyncConfig{
			DBType:    "mysql",
			SourceURI: "mysql://user:pass@localhost:3306/urldb",
			Threads:   "4",
		}

		err := exportData(cfg, dummyPath)
		require.NoError(t, err)

		require.NotEmpty(t, *executedCommands)
		assert.Contains(t, (*executedCommands)[0], "mydumper")
		assert.Contains(t, (*executedCommands)[0], "--database urldb")
	})

	t.Run("missing DBName", func(t *testing.T) {
		_, _, teardown := setupTest()
		defer teardown()

		cfg := SyncConfig{
			DBType:    "mysql",
			SourceURI: "mysql://user:pass@localhost:3306/",
			Threads:   "4",
		}

		err := exportData(cfg, dummyPath)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "MySQL export requires a target database name")
	})
}

func TestTransformData_Mysql(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	tempScript, _ := os.CreateTemp("", "transform-*.sql")
	defer os.Remove(tempScript.Name())

	cfg := SyncConfig{
		DBType:          "mysql",
		TransformURI:    "mysql://user:pass@localhost:3306/transformdb",
		TransformScript: tempScript.Name(),
		Threads:         "4",
	}

	path, err := transformData(cfg, "/tmp/test_mysql_dump")
	require.NoError(t, err)
	assert.Equal(t, "/tmp/test_mysql_dump_transformed", path)

	require.Len(t, *executedCommands, 4)
	assert.Contains(t, (*executedCommands)[0], "DROP DATABASE IF EXISTS `transformdb`; CREATE DATABASE `transformdb`;")
	assert.Contains(t, (*executedCommands)[1], "myloader")
	assert.Contains(t, (*executedCommands)[2], fmt.Sprintf("source %s", tempScript.Name()))
	assert.Contains(t, (*executedCommands)[3], "mydumper")
}

func TestRestoreData_Mysql(t *testing.T) {
	_, executedCommands, teardown := setupTest()
	defer teardown()

	// Local mock to intercept MySQL output fetches and return fake tables
	originalRunCmdWithOutput := runCmdWithOutput
	runCmdWithOutput = func(name string, args ...string) ([]byte, error) {
		*executedCommands = append(*executedCommands, name+" "+strings.Join(args, " "))
		cmdStr := strings.Join(args, " ")
		if strings.Contains(cmdStr, "SELECT table_name FROM information_schema.tables") {
			if strings.Contains(cmdStr, "destdb_tmp") {
				if strings.Contains(cmdStr, "VIEW") {
					return []byte("view1\nview2\n"), nil
				}
				return []byte("users\nposts\ncomments\n"), nil
			} else if strings.Contains(cmdStr, "destdb") {
				if strings.Contains(cmdStr, "VIEW") {
					return []byte(""), nil
				}
				return []byte("users\nposts\nold_table\n"), nil
			}
		}
		return []byte(""), nil
	}
	defer func() { runCmdWithOutput = originalRunCmdWithOutput }()

	// Local mock to intercept piped view transfer commands
	originalRunPipe := runPipe
	runPipe = func(cmd1 string, args1 []string, cmd2 string, args2 []string) error {
		*executedCommands = append(*executedCommands, cmd1+" "+strings.Join(args1, " ")+" | "+cmd2+" "+strings.Join(args2, " "))
		return nil
	}
	defer func() { runPipe = originalRunPipe }()

	cfg := SyncConfig{
		DBType:  "mysql",
		DestURI: "mysql://user:pass@localhost:3306/destdb",
		Threads: "4",
	}

	err := restoreData(cfg, "/tmp/dummy_mysql_restore")
	require.NoError(t, err)

	cmds := strings.Join(*executedCommands, "\n")

	// Verify Temp Database Setup
	assert.Contains(t, cmds, "DROP DATABASE IF EXISTS `destdb_tmp`;")
	assert.Contains(t, cmds, "CREATE DATABASE `destdb_tmp`;")

	// Verify myloader Execution
	assert.Contains(t, cmds, "myloader --host localhost --port 3306 --user user --password pass --database destdb_tmp --directory /tmp/dummy_mysql_restore")

	// Verify Live Assurance and Swap Preparation
	assert.Contains(t, cmds, "CREATE DATABASE IF NOT EXISTS `destdb`;")
	assert.Contains(t, cmds, "DROP DATABASE IF EXISTS `destdb_old`;")
	assert.Contains(t, cmds, "CREATE DATABASE `destdb_old`;")

	// Verify the complex RENAME TABLE logic
	// Our mock returned:
	// destdb_tmp tables: users, posts, comments
	// destdb live tables: users, posts, old_table
	// We expect 'users' and 'posts' to be swapped natively, 'comments' to be pulled in, and 'old_table' to be pushed out
	assert.Contains(t, cmds, "RENAME TABLE `destdb`.`users` TO `destdb_old`.`users`, `destdb_tmp`.`users` TO `destdb`.`users`, `destdb`.`posts` TO `destdb_old`.`posts`, `destdb_tmp`.`posts` TO `destdb`.`posts`, `destdb_tmp`.`comments` TO `destdb`.`comments`, `destdb`.`old_table` TO `destdb_old`.`old_table`;")

	// Verify the simulated View transfer pipe
	assert.Contains(t, cmds, "mysqldump --host localhost --port 3306 --user user --password=pass --no-data destdb_tmp view1 view2 | mysql --host localhost --port 3306 --user user --password=pass destdb")

	// Verify Final Cleanup
	assert.Contains(t, cmds, "DROP DATABASE IF EXISTS `destdb_old`;")
	assert.Contains(t, cmds, "DROP DATABASE IF EXISTS `destdb_tmp`;")
}

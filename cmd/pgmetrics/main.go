/*
 * Copyright 2023 RapidLoop, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/pborman/getopt"
	"github.com/supergoodsystems/pgmetrics"
	"github.com/supergoodsystems/pgmetrics/collector"
	"golang.org/x/term"
)

const usage = `pgmetrics collects PostgreSQL information and metrics.

Usage:
  pgmetrics [OPTION]... [DBNAME]

General options:
  -t, --timeout=SECS           individual query timeout in seconds (default: 5)
      --lock-timeout=MILLIS    lock timeout in milliseconds (default: 50)
  -i, --input=FILE             don't connect to db, instead read and display
                                   this previously saved JSON file
  -V, --version                output version information, then exit
  -?, --help[=options]         show this help, then exit
      --help=variables         list environment variables, then exit

Collection options:
  -S, --no-sizes               don't collect tablespace and relation sizes
  -c, --schema=REGEXP          collect only from schema(s) matching POSIX regexp
  -C, --exclude-schema=REGEXP  do NOT collect from schema(s) matching POSIX regexp
  -a, --table=REGEXP           collect only from table(s) matching POSIX regexp
  -A, --exclude-table=REGEXP   do NOT collect from table(s) matching POSIX regexp
      --omit=WHAT              do NOT collect the items specified as a comma-separated
                                   list of: "tables", "indexes", "sequences",
                                   "functions", "extensions", "triggers",
                                   "statements", "log", "citus", "indexdefs",
                                   "bloat"
      --sql-length=LIMIT       collect only first LIMIT characters of all SQL
                                   queries (default: 500)
      --statements-limit=LIMIT collect only utmost LIMIT number of row from
                                   pg_stat_statements (default: 100)
      --only-listed            collect info only from the databases listed as
                                   command-line args (use with Heroku)
      --all-dbs                collect info from all user databases
      --log-file               location of PostgreSQL log file
      --log-dir                read all the PostgreSQL log files in this directory
      --log-span=MINS          examine the last MINS minutes of logs (default: 5)
      --aws-rds-dbid           AWS RDS/Aurora database instance identifier
      --az-resource            Azure resource ID
      --pgpool                 collect only Pgpool metrics

Output options:
  -f, --format=FORMAT          output format; "human", "json" or "csv" (default: "human")
  -l, --toolong=SECS           for human output, transactions running longer than
                                   this are considered too long (default: 60)
  -o, --output=FILE            write output to the specified file
      --no-pager               do not invoke the pager for tty output

Connection options:
  -h, --host=HOSTNAME          database server host or socket directory
                                   (default: "%s")
  -p, --port=PORT              database server port (default: %d)
  -U, --username=USERNAME      database user name (default: "%s")
  -w, --no-password            never prompt for password
      --role=ROLE              do SET ROLE before collection

For more information, visit <https://pgmetrics.io>.
`

const variables = `Environment variables:
Usage:
  NAME=VALUE [NAME=VALUE] pgmetrics ...

  PAGER              name of external pager program
  PGAPPNAME          the application_name connection parameter
  PGDATABASE         the dbname connection parameter
  PGHOST             the host connection parameter
  PGPORT             the port connection parameter
  PGUSER             the user connection parameter
  PGPASSWORD         connection password (not recommended)
  PGPASSFILE         path to the pgpass password file
  PGSSLMODE          "disable", "require", "verify-ca", "verify-full"
  PGSSLCERT          path to client SSL certificate
  PGSSLKEY           path to secret key for client SSL certificate
  PGSSLROOTCERT      path to SSL root CA
  PGCONNECT_TIMEOUT  connection timeout in seconds

Also, the following libpq-related environment variarables are not
required/used by pgmetrics and are IGNORED:

  PGHOSTADDR, PGSERVICE,     PGSERVICEFILE, PGREALM,  PGREQUIRESSL,
  PGSSLCRL,   PGREQUIREPEER, PGKRBSRVNAME,  PGGSSLIB, PGSYSCONFDIR,
  PGLOCALEDIR

The following AWS-related environment variables are understood. For
more details about these refer to the AWS documentation.

  AWS_ACCESS_KEY_ID,   AWS_SECRET_ACCESS_KEY, AWS_REGION,
  AWS_ACCESS_KEY,      AWS_SECRET_KEY,        AWS_SESSION_TOKEN,
  AWS_DEFAULT_REGION,  AWS_PROFILE,           AWS_DEFAULT_PROFILE,
  AWS_SDK_LOAD_CONFIG, AWS_SHARED_CREDENTIALS_FILE,
  AWS_CONFIG_FILE,     AWS_CA_BUNDLE

The following Azure-related environment variables are understood. For
more details about these refer to the Azure documentation.

  AZURE_CLIENT_ID,   AZURE_TENANT_ID,   AZURE_CLIENT_SECRET,
  AZURE_USERNAME,    AZURE_PASSWORD,    AZURE_CLIENT_CERTIFICATE_PATH
`

var version string // set during build
var ignoreEnvs = []string{
	"PGHOSTADDR", "PGSERVICE", "PGSERVICEFILE", "PGREALM", "PGREQUIRESSL",
	"PGSSLCRL", "PGREQUIREPEER", "PGKRBSRVNAME", "PGGSSLIB", "PGSYSCONFDIR",
	"PGLOCALEDIR",
}

type options struct {
	// collection options
	collector.CollectConfig
	// general
	input     string
	help      string
	helpShort bool
	version   bool
	// output
	format     string
	output     string
	tooLongSec uint
	nopager    bool
	// connection
	passNone bool
}

func (o *options) defaults() {
	// collection options
	o.CollectConfig = collector.DefaultCollectConfig()
	// general
	o.input = ""
	o.help = ""
	o.helpShort = false
	o.version = false
	// output
	o.format = "human"
	o.output = ""
	o.tooLongSec = 60
	o.nopager = false
	// connection
	o.passNone = false
}

func (o *options) usage(code int) {
	fp := os.Stdout
	if code != 0 {
		fp = os.Stderr
	}
	if o.helpShort || code != 0 || o.help == "short" {
		fmt.Fprintf(fp, usage, o.CollectConfig.Host, o.CollectConfig.Port, o.CollectConfig.User)
	} else if o.help == "variables" {
		fmt.Fprint(fp, variables)
	}
	os.Exit(code)
}

func printTry() {
	fmt.Fprintf(os.Stderr, "Try \"pgmetrics --help\" for more information.\n")
}

func getRegexp(r string) (err error) {
	if len(r) > 0 {
		_, err = regexp.CompilePOSIX(r)
	}
	return
}

func (o *options) parse() (args []string) {
	// make getopt
	s := getopt.New()
	s.SetUsage(printTry)
	s.SetProgram("pgmetrics")
	// general
	s.UintVarLong(&o.CollectConfig.TimeoutSec, "timeout", 't', "")
	s.UintVarLong(&o.CollectConfig.LockTimeoutMillisec, "lock-timeout", 0, "")
	s.BoolVarLong(&o.CollectConfig.NoSizes, "no-sizes", 'S', "")
	s.StringVarLong(&o.input, "input", 'i', "")
	help := s.StringVarLong(&o.help, "help", '?', "").SetOptional()
	s.BoolVarLong(&o.version, "version", 'V', "").SetFlag()
	// collection
	s.StringVarLong(&o.CollectConfig.Schema, "schema", 'c', "")
	s.StringVarLong(&o.CollectConfig.ExclSchema, "exclude-schema", 'C', "")
	s.StringVarLong(&o.CollectConfig.Table, "table", 'a', "")
	s.StringVarLong(&o.CollectConfig.ExclTable, "exclude-table", 'A', "")
	s.ListVarLong(&o.CollectConfig.Omit, "omit", 0, "")
	s.UintVarLong(&o.CollectConfig.SQLLength, "sql-length", 0, "")
	s.UintVarLong(&o.CollectConfig.StmtsLimit, "statements-limit", 0, "")
	s.BoolVarLong(&o.CollectConfig.OnlyListedDBs, "only-listed", 0, "").SetFlag()
	s.BoolVarLong(&o.CollectConfig.AllDBs, "all-dbs", 0, "").SetFlag()
	s.StringVarLong(&o.CollectConfig.LogFile, "log-file", 0, "")
	s.StringVarLong(&o.CollectConfig.LogDir, "log-dir", 0, "")
	s.UintVarLong(&o.CollectConfig.LogSpan, "log-span", 0, "")
	s.StringVarLong(&o.CollectConfig.RDSDBIdentifier, "aws-rds-dbid", 0, "")
	s.StringVarLong(&o.CollectConfig.AzureResourceID, "az-resource", 0, "")
	s.BoolVarLong(&o.CollectConfig.Pgpool, "pgpool", 0, "").SetFlag()
	// output
	s.StringVarLong(&o.format, "format", 'f', "")
	s.StringVarLong(&o.output, "output", 'o', "")
	s.UintVarLong(&o.tooLongSec, "toolong", 'l', "")
	s.BoolVarLong(&o.nopager, "no-pager", 0, "").SetFlag()
	// connection
	s.StringVarLong(&o.CollectConfig.Host, "host", 'h', "")
	s.Uint16VarLong(&o.CollectConfig.Port, "port", 'p', "")
	s.StringVarLong(&o.CollectConfig.User, "username", 'U', "")
	s.BoolVarLong(&o.passNone, "no-password", 'w', "")
	s.StringVarLong(&o.CollectConfig.Role, "role", 0, "")

	// parse
	s.Parse(os.Args)
	if help.Seen() && o.help == "" {
		o.help = "short"
	}

	// check values
	if o.help != "" && o.help != "short" && o.help != "variables" {
		printTry()
		os.Exit(2)
	}
	if o.format != "human" && o.format != "json" && o.format != "csv" {
		fmt.Fprintln(os.Stderr, `option -f/--format must be "human", "json" or "csv"`)
		printTry()
		os.Exit(2)
	}
	if o.CollectConfig.Port == 0 {
		fmt.Fprintln(os.Stderr, "port must be between 1 and 65535")
		printTry()
		os.Exit(2)
	}
	if o.CollectConfig.TimeoutSec == 0 {
		fmt.Fprintln(os.Stderr, "timeout must be greater than 0")
		printTry()
		os.Exit(2)
	}
	if o.CollectConfig.LockTimeoutMillisec == 0 {
		fmt.Fprintln(os.Stderr, "lock-timeout must be greater than 0")
		printTry()
		os.Exit(2)
	}
	if err := getRegexp(o.CollectConfig.Schema); err != nil {
		fmt.Fprintf(os.Stderr, "bad POSIX regular expression for -c/--schema: %v\n", err)
		printTry()
		os.Exit(2)
	}
	if err := getRegexp(o.CollectConfig.ExclSchema); err != nil {
		fmt.Fprintf(os.Stderr, "bad POSIX regular expression for -C/--exclude-schema: %v\n", err)
		printTry()
		os.Exit(2)
	}
	if err := getRegexp(o.CollectConfig.Table); err != nil {
		fmt.Fprintf(os.Stderr, "bad POSIX regular expression for -a/--table: %v\n", err)
		printTry()
		os.Exit(2)
	}
	if err := getRegexp(o.CollectConfig.ExclTable); err != nil {
		fmt.Fprintf(os.Stderr, "bad POSIX regular expression for -A/--exclude-table: %v\n", err)
		printTry()
		os.Exit(2)
	}
	for _, om := range o.CollectConfig.Omit {
		if om != "tables" && om != "indexes" && om != "sequences" &&
			om != "functions" && om != "extensions" && om != "triggers" &&
			om != "statements" && om != "log" && om != "citus" &&
			om != "indexdefs" && om != "bloat" {
			fmt.Fprintf(os.Stderr, "unknown item \"%s\" in --omit option\n", om)
			printTry()
			os.Exit(2)
		}
	}

	// help action
	if o.helpShort || o.help == "short" || o.help == "variables" {
		o.usage(0)
	}

	// version action
	if o.version {
		if len(version) == 0 {
			version = "devel"
		}
		fmt.Println("pgmetrics", version)
		os.Exit(0)
	}

	// return remaining args
	return s.Args()
}

func writeTo(fd io.Writer, o options, result *pgmetrics.Model) {
	switch o.format {
	case "json":
		writeJSONTo(fd, result)
	case "csv":
		writeCSVTo(fd, result)
	default:
		writeHumanTo(fd, o, result)
	}
}

func writeJSONTo(fd io.Writer, result *pgmetrics.Model) {
	enc := json.NewEncoder(fd)
	enc.SetIndent("", "  ")
	if err := enc.Encode(result); err != nil {
		log.Fatal(err)
	}
}

func writeCSVTo(fd io.Writer, result *pgmetrics.Model) {
	w := csv.NewWriter(fd)
	if err := model2csv(result, w); err != nil {
		log.Fatal(err)
	}
	w.Flush()
}

func process(result *pgmetrics.Model, o options, args []string) {
	if o.output == "-" {
		o.output = ""
	}
	pager := os.Getenv("PAGER")
	if pager == "" {
		if _, err := exec.LookPath("less"); err == nil {
			pager = "less"
		} else if _, err := exec.LookPath("more"); err == nil {
			pager = "more"
		}
	}
	usePager := o.output == "" && !o.nopager && pager != "" &&
		term.IsTerminal(int(os.Stdout.Fd()))
	if usePager {
		cmd := exec.Command(pager)
		pagerStdin, err := cmd.StdinPipe()
		if err != nil {
			log.Fatal(err)
		}
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			log.Fatal(err)
		}
		writeTo(pagerStdin, o, result)
		pagerStdin.Close()
		_ = cmd.Wait()
	} else if o.output != "" {
		f, err := os.Create(o.output)
		if err != nil {
			log.Fatal(err)
		}
		writeTo(f, o, result)
		f.Close()
	} else {
		writeTo(os.Stdout, o, result)
	}
}

func main() {
	for _, e := range ignoreEnvs {
		os.Unsetenv(e)
	}

	var o options
	o.defaults()
	args := o.parse()
	if !o.passNone && len(o.input) == 0 && os.Getenv("PGPASSWORD") == "" {
		fmt.Fprint(os.Stderr, "Password: ")
		p, err := term.ReadPassword(int(os.Stdin.Fd()))
		if err != nil {
			os.Exit(1)
		}
		o.CollectConfig.Password = string(p)
	}

	log.SetFlags(0)
	log.SetPrefix("pgmetrics: ")

	// collect or load data
	var result *pgmetrics.Model
	if len(o.input) > 0 {
		f, err := os.Open(o.input)
		if err != nil {
			log.Fatal(err)
		}
		var obj pgmetrics.Model
		if err = json.NewDecoder(f).Decode(&obj); err != nil {
			log.Fatalf("%s: %v", o.input, err)
		}
		result = &obj
		f.Close()
	} else {
		result = collector.Collect(o.CollectConfig, args)
		// add the user agent
		if len(version) == 0 {
			result.Metadata.UserAgent = "pgmetrics/devel"
		} else {
			result.Metadata.UserAgent = "pgmetrics/" + version
		}
	}

	// process it
	process(result, o, args)
}

// Postgres version constants
const (
	pgv94 = 9_04_00
	pgv95 = 9_05_00
	pgv96 = 9_06_00
	pgv10 = 10_00_00
	pgv11 = 11_00_00
	pgv12 = 12_00_00
	pgv13 = 13_00_00
	pgv14 = 14_00_00
)

func writeHumanTo(fd io.Writer, o options, result *pgmetrics.Model) {
	if result.PgBouncer != nil {
		pgbouncerWriteHumanTo(fd, o, result)
	} else if result.Pgpool != nil {
		pgpoolWriteHumanTo(fd, o, result)
	} else {
		postgresWriteHumanTo(fd, o, result)
	}
}

func postgresWriteHumanTo(fd io.Writer, o options, result *pgmetrics.Model) {
	version := getVersion(result)
	sincePrior, _ := lsnDiff(result.RedoLSN, result.PriorLSN)
	sinceRedo, _ := lsnDiff(result.CheckpointLSN, result.RedoLSN)
	fmt.Fprintf(fd, `
 pgmetrics run at: %s

 PostgreSQL Cluster:
	 Name:                %s
	 Server Version:      %s
	 Server Started:      %s`,
		fmtTimeAndSince(result.Metadata.At),
		getSetting(result, "cluster_name"),
		getSetting(result, "server_version"),
		fmtTimeAndSince(result.StartTime),
	)
	if version >= pgv96 {
		fmt.Fprintf(fd, `
	 System Identifier:   %s
	 Timeline:            %d
	 Last Checkpoint:     %s`,
			result.SystemIdentifier,
			result.TimelineID,
			fmtTimeAndSince(result.CheckpointTime),
		)
		if result.PriorLSN != "" && result.RedoLSN != "" && result.CheckpointLSN != "" {
			fmt.Fprintf(fd, `
	 Prior LSN:           %s
	 REDO LSN:            %s (%s since Prior)
	 Checkpoint LSN:      %s (%s since REDO)`,
				result.PriorLSN,
				result.RedoLSN, humanize.IBytes(uint64(sincePrior)),
				result.CheckpointLSN, humanize.IBytes(uint64(sinceRedo)),
			)
		} else if result.PriorLSN == "" && result.RedoLSN != "" && result.CheckpointLSN != "" {
			fmt.Fprintf(fd, `
	 REDO LSN:            %s
	 Checkpoint LSN:      %s (%s since REDO)`,
				result.RedoLSN,
				result.CheckpointLSN, humanize.IBytes(uint64(sinceRedo)),
			)
		}
		fmt.Fprintf(fd, `
	 Transaction IDs:     %s`,
			fmtXIDRange(int64(result.OldestXid), int64(result.NextXid)),
		)
	}

	if result.LastXactTimestamp != 0 {
		fmt.Fprintf(fd, `
	 Last Transaction:    %s`,
			fmtTimeAndSince(result.LastXactTimestamp),
		)
	}

	if version >= pgv96 {
		fmt.Fprintf(fd, `
	 Notification Queue:  %.1f%% used`, result.NotificationQueueUsage)
	}

	fmt.Fprintf(fd, `
	 Active Backends:     %d (max %s)
	 Recovery Mode?       %s
 `,
		len(result.Backends), getSetting(result, "max_connections"),
		fmtYesNo(result.IsInRecovery),
	)

	if result.System != nil {
		reportSystem(fd, result)
	}

	if result.IsInRecovery {
		reportRecovery(fd, result)
	}

	if result.ReplicationIncoming != nil {
		reportReplicationIn(fd, result)
	}

	if len(result.ReplicationOutgoing) > 0 {
		reportReplicationOut(fd, result)
	}

	if len(result.ReplicationSlots) > 0 {
		reportReplicationSlots(fd, result, version)
	}

	reportWAL(fd, result, version)
	reportBGWriter(fd, result)
	reportBackends(fd, o.tooLongSec, result)
	reportLocks(fd, result)
	if version >= pgv96 {
		reportVacuumProgress(fd, result)
	}
	reportProgress(fd, result)
	reportDeadlocks(fd, result)
	reportAutovacuums(fd, result)
	reportRoles(fd, result)
	reportTablespaces(fd, result)
	reportDatabases(fd, result)
	reportTables(fd, result)
	fmt.Fprintln(fd)
}

func reportRecovery(fd io.Writer, result *pgmetrics.Model) {
	fmt.Fprintf(fd, `
 Recovery Status:
	 Replay paused:       %s
	 Received LSN:        %s
	 Replayed LSN:        %s%s
	 Last Replayed Txn:   %s
 `,
		fmtYesNo(result.IsWalReplayPaused),
		result.LastWALReceiveLSN,
		result.LastWALReplayLSN,
		fmtLag(result.LastWALReceiveLSN, result.LastWALReplayLSN, ""),
		fmtTimeAndSince(result.LastXActReplayTimestamp))
}

func reportReplicationIn(fd io.Writer, result *pgmetrics.Model) {
	ri := result.ReplicationIncoming
	var recvDiff string
	if d, ok := lsnDiff(ri.ReceivedLSN, ri.ReceiveStartLSN); ok && d > 0 {
		recvDiff = ", " + humanize.IBytes(uint64(d))
	}

	fmt.Fprintf(fd, `
 Incoming Replication Stats:
	 Status:              %s
	 Received LSN:        %s (started at %s%s)
	 Timeline:            %d (was %d at start)
	 Latency:             %s
	 Replication Slot:    %s
 `,
		ri.Status,
		ri.ReceivedLSN, ri.ReceiveStartLSN, recvDiff,
		ri.ReceivedTLI, ri.ReceiveStartTLI,
		fmtMicros(ri.Latency),
		ri.SlotName)
}

func reportReplicationOut(fd io.Writer, result *pgmetrics.Model) {
	routs := result.ReplicationOutgoing
	fmt.Fprintf(fd, `
 Outgoing Replication Stats:`)
	for i, r := range routs {
		var sp string
		if r.SyncPriority != -1 {
			sp = strconv.Itoa(r.SyncPriority)
		}
		fmt.Fprintf(fd, `
	 Destination #%d:
	   User:              %s
	   Application:       %s
	   Client Address:    %s
	   State:             %s
	   Started At:        %s
	   Sent LSN:          %s
	   Written Until:     %s%s
	   Flushed Until:     %s%s
	   Replayed Until:    %s%s
	   Sync Priority:     %s
	   Sync State:        %s`,
			i+1,
			r.RoleName,
			r.ApplicationName,
			r.ClientAddr,
			r.State,
			fmtTimeAndSince(r.BackendStart),
			r.SentLSN,
			r.WriteLSN, fmtLag(r.SentLSN, r.WriteLSN, "write"),
			r.FlushLSN, fmtLag(r.WriteLSN, r.FlushLSN, "flush"),
			r.ReplayLSN, fmtLag(r.FlushLSN, r.ReplayLSN, "replay"),
			sp,
			r.SyncState,
		)
	}
	fmt.Fprintln(fd)
}

func reportReplicationSlots(fd io.Writer, result *pgmetrics.Model, version int) {
	var phy, log int
	for _, r := range result.ReplicationSlots {
		if r.SlotType == "physical" {
			phy++
		} else {
			log++
		}
	}
	if phy > 0 {
		fmt.Fprintf(fd, `
 Physical Replication Slots:
 `)
		var tw tableWriter
		cols := []interface{}{"Name", "Active", "Oldest Txn ID", "Restart LSN"}
		if version >= pgv10 {
			cols = append(cols, "Temporary")
		}
		tw.add(cols...)
		for _, r := range result.ReplicationSlots {
			if r.SlotType != "physical" {
				continue
			}
			vals := []interface{}{r.SlotName, fmtYesNo(r.Active),
				fmtIntZero(r.Xmin), r.RestartLSN}
			if version >= pgv10 {
				vals = append(vals, fmtYesNo(r.Temporary))
			}
			tw.add(vals...)
		}
		tw.write(fd, "    ")
	}
	if log > 0 {
		fmt.Fprintf(fd, `
 Logical Replication Slots:
 `)
		var tw tableWriter
		cols := []interface{}{"Name", "Plugin", "Database", "Active",
			"Oldest Txn ID", "Restart LSN", "Flushed Until"}
		if version >= pgv10 {
			cols = append(cols, "Temporary")
		}
		tw.add(cols...)
		for _, r := range result.ReplicationSlots {
			if r.SlotType != "logical" {
				continue
			}
			vals := []interface{}{r.SlotName, r.Plugin, r.DBName,
				fmtYesNo(r.Active), fmtIntZero(r.Xmin), r.RestartLSN,
				r.ConfirmedFlushLSN}
			if version >= pgv10 {
				vals = append(vals, fmtYesNo(r.Temporary))
			}
			tw.add(vals...)
		}
		tw.write(fd, "    ")
	}
}

// WAL files and archiving
func reportWAL(fd io.Writer, result *pgmetrics.Model, version int) {

	archiveMode := getSetting(result, "archive_mode") == "on"
	fmt.Fprintf(fd, `
 WAL Files:
	 WAL Archiving?       %s`,
		fmtYesNo(archiveMode),
	)
	if result.WALCount != -1 {
		fmt.Fprintf(fd, `
	 WAL Files:           %d`,
			result.WALCount)
	}
	if archiveMode {
		var rate float64
		secs := result.Metadata.At - result.WALArchiving.StatsReset
		if secs > 0 {
			rate = float64(result.WALArchiving.ArchivedCount) / (float64(secs) / 60)
		}
		var rf string
		if result.WALReadyCount > -1 {
			rf = strconv.Itoa(result.WALReadyCount)
		}
		fmt.Fprintf(fd, `
	 Ready Files:         %s
	 Archive Rate:        %.2f per min
	 Last Archived:       %s
	 Last Failure:        %s
	 Totals:              %d succeeded, %d failed
	 Totals Since:        %s`,
			rf,
			rate,
			fmtTimeAndSince(result.WALArchiving.LastArchivedTime),
			fmtTimeAndSince(result.WALArchiving.LastFailedTime),
			result.WALArchiving.ArchivedCount, result.WALArchiving.FailedCount,
			fmtTimeAndSince(result.WALArchiving.StatsReset),
		)
	}
	fmt.Fprintln(fd)
	maxwalk, maxwalv := getMaxWalSize(result)
	var tw1 tableWriter
	tw1.add("Setting", "Value")
	tw1.add("wal_level", getSetting(result, "wal_level"))
	tw1.add("archive_timeout", getSetting(result, "archive_timeout"))
	tw1.add("wal_compression", getSetting(result, "wal_compression"))
	tw1.add(maxwalk, maxwalv)
	if mws := getMinWalSize(result); mws != "" {
		tw1.add("min_wal_size", mws)
	}
	tw1.add("checkpoint_timeout", getSetting(result, "checkpoint_timeout"))
	tw1.add("full_page_writes", getSetting(result, "full_page_writes"))
	if version >= pgv13 {
		tw1.add("wal_keep_size", getSettingBytes(result, "wal_keep_size", 1024*1024))
	} else {
		tw1.add("wal_keep_segments", getSetting(result, "wal_keep_segments"))
	}
	tw1.write(fd, "    ")
}

func reportBGWriter(fd io.Writer, result *pgmetrics.Model) {

	bgw := result.BGWriter
	blkSize := getBlockSize(result)
	var rate float64
	secs := result.Metadata.At - bgw.StatsReset
	ncps := bgw.CheckpointsTimed + bgw.CheckpointsRequested
	if secs > 0 {
		rate = float64(ncps) / (float64(secs) / 60)
	}
	totBuffers := bgw.BuffersCheckpoint + bgw.BuffersClean + bgw.BuffersBackend
	var pctSched, pctReq, avgWrite, rateBuffers float64
	if ncps > 0 {
		ncpsf := float64(ncps)
		pctSched = 100 * float64(bgw.CheckpointsTimed) / ncpsf
		pctReq = 100 * float64(bgw.CheckpointsRequested) / ncpsf
		avgWrite = float64(bgw.BuffersCheckpoint) * float64(blkSize) / ncpsf
	}
	if secs > 0 {
		rateBuffers = float64(totBuffers) / float64(secs)
	}
	var pctBufCP, pctBufBGW, pctBufBE float64
	if totBuffers > 0 {
		totBuffersf := float64(totBuffers)
		pctBufCP = 100 * float64(bgw.BuffersCheckpoint) / totBuffersf
		pctBufBGW = 100 * float64(bgw.BuffersClean) / totBuffersf
		pctBufBE = 100 * float64(bgw.BuffersBackend) / totBuffersf
	}
	fmt.Fprintf(fd, `
 BG Writer:
	 Checkpoint Rate:     %.2f per min
	 Average Write:       %s per checkpoint
	 Total Checkpoints:   %d sched (%.1f%%) + %d req (%.1f%%) = %d
	 Total Write:         %s, @ %s per sec
	 Buffers Allocated:   %d (%s)
	 Buffers Written:     %d chkpt (%.1f%%) + %d bgw (%.1f%%) + %d be (%.1f%%)
	 Clean Scan Stops:    %d
	 BE fsyncs:           %d
	 Counts Since:        %s
 `,
		rate,
		humanize.IBytes(uint64(avgWrite)),
		bgw.CheckpointsTimed, pctSched,
		bgw.CheckpointsRequested, pctReq, ncps,
		humanize.IBytes(uint64(blkSize)*uint64(totBuffers)),
		humanize.IBytes(uint64(float64(blkSize)*rateBuffers)),
		bgw.BuffersAlloc, humanize.IBytes(uint64(blkSize)*uint64(bgw.BuffersAlloc)),
		bgw.BuffersCheckpoint, pctBufCP,
		bgw.BuffersClean, pctBufBGW,
		bgw.BuffersBackend, pctBufBE,
		bgw.MaxWrittenClean, bgw.BuffersBackendFsync,
		fmtTimeAndSince(bgw.StatsReset),
	)

	var tw tableWriter
	tw.add("Setting", "Value")
	tw.add("bgwriter_delay", getSetting(result, "bgwriter_delay")+" msec")
	tw.add("bgwriter_flush_after", getSettingBytes(result, "bgwriter_flush_after", uint64(blkSize)))
	tw.add("bgwriter_lru_maxpages", getSetting(result, "bgwriter_lru_maxpages"))
	tw.add("bgwriter_lru_multiplier", getSetting(result, "bgwriter_lru_multiplier"))
	tw.add("block_size", getSetting(result, "block_size"))
	tw.add("checkpoint_timeout", getSetting(result, "checkpoint_timeout")+" sec")
	tw.add("checkpoint_completion_target", getSetting(result, "checkpoint_completion_target"))
	tw.write(fd, "    ")
}

func isWaitingLock(be *pgmetrics.Backend) bool {
	if be.WaitEventType == "waiting" && be.WaitEvent == "waiting" {
		return true // before v9.6, see collector.getActivity94
	}
	return be.WaitEventType == "Lock"
}

func isWaitingOther(be *pgmetrics.Backend) bool {
	return len(be.WaitEventType) > 0 && be.WaitEventType != "Lock" && be.WaitEventType != "waiting"
}

func reportBackends(fd io.Writer, tooLongSecs uint, result *pgmetrics.Model) {
	n := len(result.Backends)
	max := getSettingInt(result, "max_connections")
	isTooLong := func(be *pgmetrics.Backend) bool {
		return be.XactStart > 0 && result.Metadata.At-be.XactStart > int64(tooLongSecs)
	}
	var waitingLocks, waitingOther, idlexact, toolong int
	for _, be := range result.Backends {
		if isWaitingLock(&be) {
			waitingLocks++
		}
		if isWaitingOther(&be) {
			waitingOther++
		}
		if strings.HasPrefix(be.State, "idle in transaction") {
			idlexact++
		}
		if isTooLong(&be) {
			toolong++
		}
	}

	// header
	fmt.Fprintf(fd, `
 Backends:
	 Total Backends:      %d (%.1f%% of max %d)
	 Problematic:         %d waiting on locks, %d waiting on other, %d xact too long, %d idle in xact`,
		n, 100*safeDiv(int64(n), int64(max)), max,
		waitingLocks, waitingOther, toolong, idlexact,
	)

	// "waiting for locks" backends
	if waitingLocks > 0 {
		fmt.Fprint(fd, `
	 Waiting for Locks:
 `)
		var tw tableWriter
		tw.add("PID", "User", "App", "Client Addr", "Database", "Wait", "Query Start")
		for _, be := range result.Backends {
			if isWaitingLock(&be) {
				tw.add(be.PID, be.RoleName, be.ApplicationName, be.ClientAddr,
					be.DBName, be.WaitEventType+" / "+be.WaitEvent,
					fmtTime(be.QueryStart))
			}
		}
		tw.write(fd, "      ")
	}

	// "other waiting" backends
	if waitingOther > 0 {
		fmt.Fprint(fd, `
	 Other Waiting Backends:
 `)
		var tw tableWriter
		tw.add("PID", "User", "App", "Client Addr", "Database", "Wait", "Query Start")
		for _, be := range result.Backends {
			if isWaitingOther(&be) {
				tw.add(be.PID, be.RoleName, be.ApplicationName, be.ClientAddr,
					be.DBName, be.WaitEventType+" / "+be.WaitEvent,
					fmtTime(be.QueryStart))
			}
		}
		tw.write(fd, "      ")
	}

	// long running xacts
	if toolong > 0 {
		fmt.Fprintf(fd, `
	 Long Running (>%d sec) Transactions:
 `, tooLongSecs)
		var tw tableWriter
		tw.add("PID", "User", "App", "Client Addr", "Database", "Transaction Start")
		for _, be := range result.Backends {
			if isTooLong(&be) {
				tw.add(be.PID, be.RoleName, be.ApplicationName, be.ClientAddr, be.DBName,
					fmtTimeAndSince(be.XactStart))
			}
		}
		tw.write(fd, "      ")
	}

	// idle in xact backends
	if idlexact > 0 {
		fmt.Fprint(fd, `
	 Idling in Transaction:
 `)
		var tw tableWriter
		tw.add("PID", "User", "App", "Client Addr", "Database", "Aborted?", "State Change")
		for _, be := range result.Backends {
			if strings.HasPrefix(be.State, "idle in transaction") {
				tw.add(be.PID, be.RoleName, be.ApplicationName, be.ClientAddr,
					be.DBName, fmtYesNo(strings.Contains(be.State, "aborted")),
					fmtTime(be.StateChange))
			}
		}
		tw.write(fd, "      ")
	}

	if waitingOther+waitingLocks+idlexact+toolong == 0 {
		fmt.Fprintln(fd)
	}
}

type lockCount struct {
	notGranted int
	total      int
}

func reportLocks(fd io.Writer, result *pgmetrics.Model) {
	if len(result.Locks) == 0 {
		return
	}

	c := make(map[string]*lockCount)
	for _, l := range result.Locks {
		lc, ok := c[l.LockType]
		if !ok {
			lc = &lockCount{}
			c[l.LockType] = lc
		}
		if !l.Granted {
			lc.notGranted++
		}
		lc.total++
	}
	lt := make([]string, 0, len(c))
	for k := range c {
		lt = append(lt, k)
	}
	sort.Strings(lt)

	fmt.Fprint(fd, `
 Locks:
 `)
	var tw tableWriter
	tw.add("Lock Type", "Not Granted", "Total")
	var tot1, tot2 int
	for _, t := range lt {
		lc, ok := c[t]
		if !ok || lc == nil {
			continue
		}
		tw.add(t, lc.notGranted, lc.total)
		tot1 += lc.notGranted
		tot2 += lc.total
	}
	tw.add("", tot1, tot2)
	tw.hasFooter = true
	tw.write(fd, "    ")
}

func reportVacuumProgress(fd io.Writer, result *pgmetrics.Model) {
	fmt.Fprint(fd, `
 Vacuum Progress:`)
	if len(result.VacuumProgress) > 0 {
		for i, v := range result.VacuumProgress {
			sp := fmt.Sprintf("%d of %d (%.1f%% complete)", v.HeapBlksScanned,
				v.HeapBlksTotal, 100*safeDiv(v.HeapBlksScanned, v.HeapBlksTotal))
			fmt.Fprintf(fd, `
	 Vacuum Process #%d:
	   Phase:             %s
	   Database:          %s
	   Table:             %s
	   Scan Progress:     %s
	   Heap Blks Vac'ed:  %d of %d
	   Idx Vac Cycles:    %d
	   Dead Tuples:       %d
	   Dead Tuples Max:   %d`,
				i+1,
				v.Phase,
				v.DBName,
				v.TableName,
				sp,
				v.HeapBlksVacuumed, v.HeapBlksTotal,
				v.IndexVacuumCount,
				v.NumDeadTuples,
				v.MaxDeadTuples,
			)
		}
	} else {
		fmt.Fprint(fd, `
	 No manual or auto vacuum jobs in progress.`)
	}
	fmt.Fprintln(fd)

	// settings
	var tw tableWriter
	add := func(s string) { tw.add(s, getSetting(result, s)) }
	tw.add("Setting", "Value")
	tw.add("maintenance_work_mem", getSettingBytes(result, "maintenance_work_mem", 1024))
	add("autovacuum")
	add("autovacuum_analyze_threshold")
	add("autovacuum_vacuum_threshold")
	add("autovacuum_freeze_max_age")
	add("autovacuum_max_workers")
	tw.add("autovacuum_naptime", getSetting(result, "autovacuum_naptime")+" sec")
	add("vacuum_freeze_min_age")
	add("vacuum_freeze_table_age")
	tw.write(fd, "    ")
}

func reportProgress(fd io.Writer, result *pgmetrics.Model) {
	if len(result.VacuumProgress)+len(result.AnalyzeProgress)+
		len(result.BasebackupProgress)+len(result.ClusterProgress)+
		len(result.CopyProgress)+len(result.CreateIndexProgress) == 0 {
		return // no jobs in progress
	}

	var tw tableWriter
	tw.add("Job", "Backend", "Working On", "Status")

	// analyze
	for _, a := range result.AnalyzeProgress {
		object := "?"
		if t := result.TableByOID(a.TableOID); t != nil {
			object = a.DBName + "." + t.Name
		}
		tw.add("ANALYZE", a.PID, object, a.Phase)
	}

	// basebackup
	for _, b := range result.BasebackupProgress {
		tw.add("BASEBACKUP", b.PID, "", b.Phase)
	}

	// cluster / vacuum full
	for _, c := range result.ClusterProgress {
		object := "?"
		if t := result.TableByOID(c.TableOID); t != nil {
			object = c.DBName + "." + t.Name
		}
		tw.add(c.Command, c.PID, object, c.Phase)
	}

	// copy from / copy to
	for _, c := range result.CopyProgress {
		object := "(query)"
		if t := result.TableByOID(c.TableOID); t != nil {
			object = c.DBName + "." + t.Name
		}
		tw.add(c.Command, c.PID, object, "")
	}

	// create index (concurrently) / reindex (concurrently)
	for _, c := range result.CreateIndexProgress {
		object := "?"
		if t := result.TableByOID(c.TableOID); t != nil {
			object = c.DBName + "." + t.Name
			if idx := result.IndexByOID(c.IndexOID); idx != nil {
				object += "." + idx.Name
			}
		}
		tw.add(c.Command, c.PID, object, c.Phase)
	}

	// vacuum
	for _, v := range result.VacuumProgress {
		object := "?"
		if t := result.TableByOID(v.TableOID); t != nil {
			object = v.DBName + "." + t.Name
		}
		tw.add("VACUUM", v.PID, object, v.Phase)
	}

	fmt.Fprint(fd, `
 Jobs In Progress:
 `)
	tw.write(fd, "    ")
}

func reportDeadlocks(fd io.Writer, result *pgmetrics.Model) {
	if len(result.Deadlocks) == 0 {
		return // no recent deadlocks
	}

	var tw tableWriter
	tw.add("At", "Detail")
	for i, d := range result.Deadlocks {
		detail := strings.ReplaceAll(d.Detail, "\r", "")
		detail = strings.TrimSpace(detail)
		lines := strings.Split(detail, "\n")
		if n := len(lines); n > 0 {
			tw.add(fmtTime(d.At), lines[0])
			for j := 1; j < n; j++ {
				tw.add("", lines[j])
			}
			if i < len(result.Deadlocks)-1 {
				tw.add(twLine, 0)
			}
		}
	}

	fmt.Fprint(fd, `
 Recent Deadlocks:
 `)
	tw.write(fd, "    ")
}

func reportAutovacuums(fd io.Writer, result *pgmetrics.Model) {
	if len(result.AutoVacuums) == 0 {
		return // no recent autovacuums
	}

	var tw tableWriter
	tw.add("At", "Table", "Time Taken (sec)")
	for _, a := range result.AutoVacuums {
		tw.add(fmtTime(a.At), a.Table, fmt.Sprintf("%.1f", a.Elapsed))
	}

	fmt.Fprint(fd, `
 Recent Autovacuums:
 `)
	tw.write(fd, "    ")
}

func reportRoles(fd io.Writer, result *pgmetrics.Model) {
	fmt.Fprint(fd, `
 Roles:
 `)
	var tw tableWriter
	tw.add("Name", "Login", "Repl", "Super", "Creat Rol", "Creat DB", "Bypass RLS", "Inherit", "Expires", "Member Of")
	for _, r := range result.Roles {
		tw.add(
			r.Name,
			fmtYesBlank(r.Rolcanlogin),
			fmtYesBlank(r.Rolreplication),
			fmtYesBlank(r.Rolsuper),
			fmtYesBlank(r.Rolcreaterole),
			fmtYesBlank(r.Rolcreatedb),
			fmtYesBlank(r.Rolbypassrls),
			fmtYesBlank(r.Rolinherit),
			fmtTime(r.Rolvaliduntil),
			strings.Join(r.MemberOf, ", "),
		)
	}
	tw.write(fd, "    ")
}

func reportTablespaces(fd io.Writer, result *pgmetrics.Model) {
	fmt.Fprint(fd, `
 Tablespaces:
 `)
	var tw tableWriter
	if result.Metadata.Local {
		tw.add("Name", "Owner", "Location", "Size", "Disk Used", "Inode Used")
	} else {
		tw.add("Name", "Owner", "Location", "Size")
	}
	for _, t := range result.Tablespaces {
		var s, du, iu string
		if t.Size != -1 {
			s = humanize.IBytes(uint64(t.Size))
		}
		if result.Metadata.Local && t.DiskUsed > 0 && t.DiskTotal > 0 {
			du = fmt.Sprintf("%s (%.1f%%) of %s",
				humanize.IBytes(uint64(t.DiskUsed)),
				100*safeDiv(t.DiskUsed, t.DiskTotal),
				humanize.IBytes(uint64(t.DiskTotal)))
		}
		if result.Metadata.Local && t.InodesUsed > 0 && t.InodesTotal > 0 {
			iu = fmt.Sprintf("%d (%.1f%%) of %d",
				t.InodesUsed,
				100*safeDiv(t.InodesUsed, t.InodesTotal),
				t.InodesTotal)
		}
		if (t.Name == "pg_default" || t.Name == "pg_global") && t.Location != "" {
			t.Location = "$PGDATA = " + t.Location
		}
		if result.Metadata.Local {
			tw.add(t.Name, t.Owner, t.Location, s, du, iu)
		} else {
			tw.add(t.Name, t.Owner, t.Location, s)
		}
	}
	tw.write(fd, "    ")
}

func getTablespaceName(oid int, result *pgmetrics.Model) string {
	for _, t := range result.Tablespaces {
		if t.OID == oid {
			return t.Name
		}
	}
	return ""
}

func getRoleName(oid int, result *pgmetrics.Model) string {
	for _, r := range result.Roles {
		if r.OID == oid {
			return r.Name
		}
	}
	return ""
}

func fmtConns(d *pgmetrics.Database) string {
	if d.DatConnLimit < 0 {
		return fmt.Sprintf("%d (no max limit)", d.NumBackends)
	}
	pct := 100 * safeDiv(int64(d.NumBackends), int64(d.DatConnLimit))
	return fmt.Sprintf("%d (%.1f%%) of %d", d.NumBackends, pct, d.DatConnLimit)
}

func reportDatabases(fd io.Writer, result *pgmetrics.Model) {
	for i, d := range result.Databases {
		fmt.Fprintf(fd, `
 Database #%d:
	 Name:                %s
	 Owner:               %s
	 Tablespace:          %s
	 Connections:         %s
	 Frozen Xid Age:      %d
	 Transactions:        %d (%.1f%%) commits, %d (%.1f%%) rollbacks
	 Cache Hits:          %.1f%%
	 Rows Changed:        ins %.1f%%, upd %.1f%%, del %.1f%%
	 Total Temp:          %s in %d files
	 Problems:            %d deadlocks, %d conflicts
	 Totals Since:        %s`,
			i+1,
			d.Name,
			getRoleName(d.DatDBA, result),
			getTablespaceName(d.DatTablespace, result),
			fmtConns(&d),
			d.AgeDatFrozenXid,
			d.XactCommit, 100*safeDiv(d.XactCommit, d.XactCommit+d.XactRollback),
			d.XactRollback, 100*safeDiv(d.XactRollback, d.XactCommit+d.XactRollback),
			100*safeDiv(d.BlksHit, d.BlksHit+d.BlksRead),
			100*safeDiv(d.TupInserted, d.TupInserted+d.TupUpdated+d.TupDeleted),
			100*safeDiv(d.TupUpdated, d.TupInserted+d.TupUpdated+d.TupDeleted),
			100*safeDiv(d.TupDeleted, d.TupInserted+d.TupUpdated+d.TupDeleted),
			humanize.IBytes(uint64(d.TempBytes)), d.TempFiles,
			d.Deadlocks, d.Conflicts,
			fmtTimeAndSince(d.StatsReset),
		)
		if d.Size != -1 {
			fmt.Fprintf(fd, `
	 Size:                %s`, humanize.IBytes(uint64(d.Size)))
		}
		fmt.Fprintln(fd)

		gap := false
		if sqs := filterSequencesByDB(result, d.Name); len(sqs) > 0 {
			fmt.Fprint(fd, `    Sequences:
 `)
			var tw tableWriter
			tw.add("Sequence", "Cache Hits")
			for _, sq := range sqs {
				tw.add(sq.Name, fmtPct(sq.BlksHit, sq.BlksHit+sq.BlksRead))
			}
			tw.write(fd, "      ")
			gap = true
		}

		if ufs := filterUserFuncsByDB(result, d.Name); len(ufs) > 0 {
			if gap {
				fmt.Fprintln(fd)
			}
			fmt.Fprint(fd, `    Tracked Functions:
 `)
			var tw tableWriter
			tw.add("Function", "Calls", "Time (self)", "Time (self+children)")
			for _, uf := range ufs {
				tw.add(
					uf.Name,
					uf.Calls,
					time.Duration(uf.SelfTime*1e6),
					time.Duration(uf.TotalTime*1e6),
				)
			}
			tw.write(fd, "      ")
			gap = true
		}

		if exts := filterExtensionsByDB(result, d.Name); len(exts) > 0 {
			if gap {
				fmt.Fprintln(fd)
			}
			fmt.Fprint(fd, `    Installed Extensions:
 `)
			var tw tableWriter
			tw.add("Name", "Version", "Comment")
			for _, ext := range exts {
				tw.add(ext.Name, ext.InstalledVersion, ext.Comment)
			}
			tw.write(fd, "      ")
			gap = true
		}

		if dts := filterTriggersByDB(result, d.Name); len(dts) > 0 {
			if gap {
				fmt.Fprintln(fd)
			}
			fmt.Fprint(fd, `    Disabled Triggers:
 `)
			var tw tableWriter
			tw.add("Name", "Table", "Procedure")
			for _, dt := range dts {
				tw.add(
					dt.Name,
					dt.SchemaName+"."+dt.TableName,
					dt.ProcName,
				)
			}
			tw.write(fd, "      ")
			gap = true
		}

		if ss := filterStatementsByDB(result, d.Name); len(ss) > 0 {
			if gap {
				fmt.Fprintln(fd)
			}
			fmt.Fprint(fd, `    Slow Queries:
 `)
			var tw tableWriter
			tw.add("Calls", "Avg Time", "Total Time", "Rows/Call", "Query")
			for _, s := range ss {
				var rpc int64
				if s.Calls > 0 {
					rpc = s.Rows / s.Calls
				}
				tw.add(
					s.Calls,
					prepmsec(s.TotalTime/float64(s.Calls)),
					prepmsec(s.TotalTime),
					rpc,
					prepQ(s.Query),
				)
			}
			tw.write(fd, "      ")
			gap = true
		}

		if pp := filterPublicationsByDB(result, d.Name); len(pp) > 0 {
			if gap {
				fmt.Fprintln(fd)
			}
			fmt.Fprintf(fd, `    Logical Replication Publications:
 `)
			var tw tableWriter
			tw.add("Name", "All Tables?", "Propagate", "Tables")
			for _, p := range pp {
				tw.add(
					p.Name,
					fmtYesNo(p.AllTables),
					fmtPropagate(p.Insert, p.Update, p.Delete),
					p.TableCount,
				)
			}
			tw.write(fd, "      ")
			gap = true
		}

		if ss := filterSubscriptionsByDB(result, d.Name); len(ss) > 0 {
			if gap {
				fmt.Fprintln(fd)
			}
			fmt.Fprintf(fd, `    Logical Replication Subscriptions:
 `)
			for i, s := range ss {
				fmt.Fprintf(fd, `      Subscription #%d:
		 Name:              %s
		 Enabled?           %s
		 Publications:      %d
		 Tables:            %d
		 Workers:           %d
		 Received Until:    %s
		 Latency:           %s
 `,
					i+1,
					s.Name,
					fmtYesNo(s.Enabled),
					s.PubCount,
					s.TableCount,
					s.WorkerCount,
					s.ReceivedLSN,
					fmtMicros(s.Latency),
				)
			}
			gap = true
		}

		if ls := filterLocksByDB(result, d.Name); hasBlockedQueries(ls) {
			if gap {
				fmt.Fprintln(fd)
			}
			fmt.Fprintf(fd, `    Blocked Queries:
 `)
			count := 0
			for _, l := range ls {
				if l.Granted {
					continue
				}
				be := getBE(result, l.PID)
				if be == nil {
					continue
				}
				count++
				fmt.Fprintf(fd, `      Blocked Query #%d:
		 Query:             %s
		 Started By:        %s
		 Waiting Since:     %s
 `,
					count,
					prepQ(be.Query),
					getBEClient(be),
					fmtTimeAndSince(be.StateChange))
				if result.BlockingPIDs == nil {
					continue
				}
				pids, ok := result.BlockingPIDs[l.PID]
				if !ok || len(pids) == 0 {
					continue
				}
				for _, b := range pids {
					bbe := getBE(result, b)
					if bbe == nil {
						continue
					}
					fmt.Fprintf(fd, `        Waiting For:
		   Query:             %s
		   Lock:              %s
		   Started By:        %s
 `,
						prepQ(bbe.Query),
						getLockDesc(l, result),
						getBEClient(bbe),
					)
				}
			}
			gap = true
		}
	}
}

func hasBlockedQueries(locks []*pgmetrics.Lock) bool {
	for _, l := range locks {
		if l != nil && !l.Granted {
			return true
		}
	}
	return false
}

func getBE(result *pgmetrics.Model, pid int) *pgmetrics.Backend {
	for i, be := range result.Backends {
		if be.PID == pid {
			return &result.Backends[i]
		}
	}
	return nil
}

func getBEClient(be *pgmetrics.Backend) string {
	// role@client:db (PID pid)
	// appname role@client:db (PID pid)
	var out string
	if len(be.ApplicationName) > 0 {
		out = be.ApplicationName + " "
	}
	out += be.RoleName
	if len(be.ClientAddr) > 0 {
		c := strings.TrimSuffix(be.ClientAddr, "/128")
		c = strings.TrimSuffix(c, "/32")
		out += "@" + c
	}
	out += fmt.Sprintf("/%s (PID %d)", be.DBName, be.PID)
	return out
}

func getLockDesc(l *pgmetrics.Lock, result *pgmetrics.Model) (out string) {
	out = l.LockType + ", " + l.Mode
	if l.LockType == "relation" {
		// search tables
		if t := result.TableByOID(l.RelationOID); t != nil {
			out += ", table " + t.SchemaName + "." + t.Name
		} else {
			// else search indexes
			for _, idx := range result.Indexes {
				if idx.OID == l.RelationOID {
					out += ", index " + idx.SchemaName + "." + idx.Name
					break
				}
			}
		}
	}
	return
}

const stmtSQLDisplayLength = 50

func prepQ(s string) string {
	if len(s) > stmtSQLDisplayLength {
		s = s[:stmtSQLDisplayLength]
	}
	return strings.Map(smap, s)
}

func smap(r rune) rune {
	if r == '\r' || r == '\n' || r == '\t' {
		return ' '
	}
	return r
}

func prepmsec(ms float64) string {
	return time.Duration(1e6 * ms).Truncate(time.Millisecond).String()
}

func filterSequencesByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Sequence) {
	for i := range result.Sequences {
		if s := &result.Sequences[i]; s.DBName == db {
			out = append(out, s)
		}
	}
	return
}

func filterUserFuncsByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.UserFunction) {
	for i := range result.UserFunctions {
		if uf := &result.UserFunctions[i]; uf.DBName == db {
			out = append(out, uf)
		}
	}
	return
}

func filterExtensionsByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Extension) {
	for i := range result.Extensions {
		if e := &result.Extensions[i]; e.DBName == db {
			out = append(out, e)
		}
	}
	return
}

func filterTriggersByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Trigger) {
	for i := range result.DisabledTriggers {
		if t := &result.DisabledTriggers[i]; t.DBName == db {
			out = append(out, t)
		}
	}
	return
}

func filterStatementsByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Statement) {
	for i := range result.Statements {
		if s := &result.Statements[i]; s.DBName == db {
			out = append(out, s)
		}
	}
	return
}

func filterTablesByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Table) {
	for i := range result.Tables {
		if t := &result.Tables[i]; t.DBName == db {
			out = append(out, t)
		}
	}
	return
}

func filterPublicationsByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Publication) {
	for i := range result.Publications {
		if p := &result.Publications[i]; p.DBName == db {
			out = append(out, p)
		}
	}
	return
}

func filterSubscriptionsByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Subscription) {
	for i := range result.Subscriptions {
		if s := &result.Subscriptions[i]; s.DBName == db {
			out = append(out, s)
		}
	}
	return
}

func filterLocksByDB(result *pgmetrics.Model, db string) (out []*pgmetrics.Lock) {
	for i := range result.Locks {
		if l := &result.Locks[i]; l.DBName == db {
			out = append(out, l)
		}
	}
	return
}

// Duh. Did anyone say generics?

func fmtPct(a, b int64) string {
	if b == 0 {
		return ""
	}
	return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
}

func fmtCountAndTime(n, last int64) string {
	if n == 0 || last == 0 {
		return "never"
	}
	return fmt.Sprintf("%d, last %s", n, fmtSince(last))
}

func filterIndexesByTable(result *pgmetrics.Model, db, schema, table string) (out []*pgmetrics.Index) {
	for i := range result.Indexes {
		idx := &result.Indexes[i]
		if idx.DBName == db && idx.SchemaName == schema && idx.TableName == table {
			out = append(out, idx)
		}
	}
	return
}

func reportTables(fd io.Writer, result *pgmetrics.Model) {
	for _, db := range result.Metadata.CollectedDBs {
		tables := filterTablesByDB(result, db)
		if len(tables) == 0 {
			continue
		}
		for i, t := range tables {
			nTup := t.NLiveTup + t.NDeadTup
			nTupChanged := t.NTupIns + t.NTupUpd + t.NTupDel
			attrs := tableAttrs(t)
			fmt.Fprintf(fd, `
 Table #%d in "%s":
	 Name:                %s.%s.%s`,
				i+1,
				db,
				db, t.SchemaName, t.Name)
			if len(attrs) > 0 {
				fmt.Fprintf(fd, `
	 Attributes:          %s`, attrs)
			}
			if len(t.ParentName) > 0 {
				if len(t.PartitionCV) > 0 {
					fmt.Fprintf(fd, `
	 Partition of:        %s, %s`, t.ParentName, t.PartitionCV)
				} else {
					fmt.Fprintf(fd, `
	 Inherits from:       %s`, t.ParentName)
				}
			}
			if len(t.TablespaceName) > 0 {
				fmt.Fprintf(fd, `
	 Tablespace:          %s`, t.TablespaceName)
			}
			fmt.Fprintf(fd, `
	 Columns:             %d
	 Manual Vacuums:      %s
	 Manual Analyze:      %s
	 Auto Vacuums:        %s
	 Auto Analyze:        %s
	 Post-Analyze:        %.1f%% est. rows modified
	 Row Estimate:        %.1f%% live of total %d
	 Rows Changed:        ins %.1f%%, upd %.1f%%, del %.1f%%
	 HOT Updates:         %.1f%% of all updates
	 Seq Scans:           %d, %.1f rows/scan
	 Idx Scans:           %d, %.1f rows/scan
	 Cache Hits:          %.1f%% (idx=%.1f%%)`,
				t.RelNAtts,
				fmtCountAndTime(t.VacuumCount, t.LastVacuum),
				fmtCountAndTime(t.AnalyzeCount, t.LastAnalyze),
				fmtCountAndTime(t.AutovacuumCount, t.LastAutovacuum),
				fmtCountAndTime(t.AutoanalyzeCount, t.LastAutoanalyze),
				100*safeDiv(t.NModSinceAnalyze, nTup),
				100*safeDiv(t.NLiveTup, nTup), nTup,
				100*safeDiv(t.NTupIns, nTupChanged),
				100*safeDiv(t.NTupHotUpd, nTupChanged),
				100*safeDiv(t.NTupDel, nTupChanged),
				100*safeDiv(t.NTupHotUpd, t.NTupUpd),
				t.SeqScan, safeDiv(t.SeqTupRead, t.SeqScan),
				t.IdxScan, safeDiv(t.IdxTupFetch, t.IdxScan),
				100*safeDiv(t.HeapBlksHit+t.ToastBlksHit+t.TidxBlksHit,
					t.HeapBlksHit+t.HeapBlksRead+
						t.ToastBlksHit+t.ToastBlksRead+
						t.TidxBlksHit+t.TidxBlksRead),
				100*safeDiv(t.IdxBlksHit, t.IdxBlksHit+t.IdxBlksRead),
			)
			if t.Size != -1 {
				fmt.Fprintf(fd, `
	 Size:                %s`, humanize.IBytes(uint64(t.Size)))
			}
			if t.Bloat != -1 {
				if t.Size != -1 {
					fmt.Fprintf(fd, `
	 Bloat:               %s (%.1f%%)`,
						humanize.IBytes(uint64(t.Bloat)),
						100*safeDiv(t.Bloat, t.Size))
				} else {
					fmt.Fprintf(fd, `
	 Bloat:               %s`, humanize.IBytes(uint64(t.Bloat)))
				}
			}
			if acls := parseACL(t.ACL); len(acls) > 0 {
				fmt.Fprintf(fd, `
	 ACL:
 `)
				var tw tableWriter
				tw.add("Role", "Privileges", "Granted By")
				for _, a := range acls {
					tw.add(a.role, strings.Join(a.privs, ", "), a.grantor)
				}
				tw.write(fd, "      ")
			}
			fmt.Fprintln(fd)

			idxs := filterIndexesByTable(result, db, t.SchemaName, t.Name)
			if len(idxs) == 0 {
				continue
			}
			var tw tableWriter
			tw.add("Index", "Type", "Size", "Bloat", "Cache Hits", "Scans", "Rows Read/Scan", "Rows Fetched/Scan")
			for _, idx := range idxs {
				var sz, bloat string
				if idx.Size != -1 {
					sz = humanize.IBytes(uint64(idx.Size))
				}
				if idx.Bloat != -1 {
					if idx.Size != -1 {
						bloat = fmt.Sprintf("%s (%.1f%%)",
							humanize.IBytes(uint64(idx.Bloat)),
							100*safeDiv(idx.Bloat, idx.Size))
					} else {
						bloat = humanize.IBytes(uint64(idx.Bloat))
					}
				}
				tw.add(
					idx.Name,
					idx.AMName,
					sz,
					bloat,
					fmtPct(idx.IdxBlksHit, idx.IdxBlksHit+idx.IdxBlksRead),
					idx.IdxScan,
					fmt.Sprintf("%.1f", safeDiv(idx.IdxTupRead, idx.IdxScan)),
					fmt.Sprintf("%.1f", safeDiv(idx.IdxTupFetch, idx.IdxScan)),
				)
			}
			tw.write(fd, "    ")
		}
	}
}

func tableAttrs(t *pgmetrics.Table) string {
	var parts []string
	if t.RelPersistence == "u" {
		parts = append(parts, "unlogged")
	} else if t.RelPersistence == "t" {
		parts = append(parts, "temporary")
	}
	if t.RelKind == "m" {
		parts = append(parts, "materialized view")
	} else if t.RelKind == "p" {
		parts = append(parts, "partition parent")
	}
	if t.RelIsPartition {
		parts = append(parts, "partition")
	}
	return strings.Join(parts, ", ")
}

func reportSystem(fd io.Writer, result *pgmetrics.Model) {
	s := result.System
	fmt.Fprintf(fd, `
 System Information:
	 Hostname:            %s
	 CPU Cores:           %d x %s
	 Load Average:        %.2f
	 Memory:              used=%s, free=%s, buff=%s, cache=%s
	 Swap:                used=%s, free=%s
 `,
		s.Hostname,
		s.NumCores, s.CPUModel,
		s.LoadAvg,
		humanize.IBytes(uint64(s.MemUsed)),
		humanize.IBytes(uint64(s.MemFree)),
		humanize.IBytes(uint64(s.MemBuffers)),
		humanize.IBytes(uint64(s.MemCached)),
		humanize.IBytes(uint64(s.SwapUsed)),
		humanize.IBytes(uint64(s.SwapFree)),
	)
	var tw tableWriter
	tw.add("Setting", "Value")
	add := func(k string) { tw.add(k, getSetting(result, k)) }
	addBytes := func(k string, f uint64) { tw.add(k, getSettingBytes(result, k, f)) }
	addBytes("shared_buffers", 8192)
	addBytes("work_mem", 1024)
	addBytes("maintenance_work_mem", 1024)
	addBytes("temp_buffers", 8192)
	if v := getSetting(result, "autovacuum_work_mem"); v == "-1" {
		tw.add("autovacuum_work_mem", v)
	} else {
		addBytes("autovacuum_work_mem", 1024)
	}
	if v := getSetting(result, "temp_file_limit"); v == "-1" {
		tw.add("temp_file_limit", v)
	} else {
		addBytes("temp_file_limit", 1024)
	}
	add("max_worker_processes")
	add("autovacuum_max_workers")
	add("max_parallel_workers_per_gather")
	add("effective_io_concurrency")
	tw.write(fd, "    ")
}

//------------------------------------------------------------------------------
// pgbouncer

func pgbouncerWriteHumanTo(fd io.Writer, o options, result *pgmetrics.Model) {
	var tw tableWriter
	fmt.Fprintf(fd, `
 pgmetrics run at: %s
 `,
		fmtTimeAndSince(result.Metadata.At),
	)

	// databases
	fmt.Fprintf(fd, `
 PgBouncer Databases:
 `)
	tw.clear()
	tw.add("Database", "Maps To", "Paused?", "Disabled?", "Clients", "Xacts*", "Queries*", "Client Wait*")
	var dbs []string
	for _, db := range result.PgBouncer.Databases {
		dbs = append(dbs, db.Database)
	}
	sort.Strings(dbs)
	for _, name := range dbs {
		cols := []interface{}{name}
		for _, db := range result.PgBouncer.Databases {
			if db.Database == name {
				if name == "pgbouncer" {
					cols = append(cols, "(internal)")
				} else {
					host := db.Host
					if strings.Contains(host, ":") {
						host = "[" + host + "]"
					}
					user := db.User
					if len(user) > 0 {
						user += "@"
					}
					cols = append(cols, fmt.Sprintf("%s%s:%d/%s", user, host, db.Port, db.SourceDatabase))
				}
				cols = append(cols, fmtYesNo(db.Paused), fmtYesNo(db.Disabled))
				if db.MaxConn != 0 {
					cols = append(cols, fmt.Sprintf("%d of %d", db.CurrConn, db.MaxConn))
				} else {
					cols = append(cols, db.CurrConn)
				}
				break
			}
		}
		found := false
		for _, s := range result.PgBouncer.Stats {
			if s.Database == name {
				cols = append(cols,
					s.TotalXactCount,
					s.TotalQueryCount,
					time.Duration(s.TotalWaitTime*1e9).Truncate(time.Millisecond))
				found = true
				break
			}
		}
		if !found {
			cols = append(cols, "0", "0", "0s")
		}
		tw.add(cols...)
	}
	w := tw.write(fd, "    ")
	msg := "* = cumulative values since start of PgBouncer"
	fmt.Fprintf(fd, `%*s
 `,
		w, msg)

	// pools
	fmt.Fprintf(fd, `
 PgBouncer Pools:
 `)
	tw.clear()
	tw.add("User", "Database", "Mode", "Client Conns", "Server Conns", "Max Wait")
	for _, p := range result.PgBouncer.Pools {
		tw.add(
			p.UserName,
			p.Database,
			p.Mode,
			fmt.Sprintf("%d actv, %d wtng", p.ClActive, p.ClWaiting),
			fmt.Sprintf("%d actv, %d idle, %d othr", p.SvActive, p.SvIdle, p.SvUsed+p.SvTested),
			time.Duration(p.MaxWait*1e9).Truncate(time.Millisecond))
	}
	tw.write(fd, "    ")

	// client connections
	r := result.PgBouncer
	fmt.Fprintf(fd, `
 Current Connections:
	 Clients: %d active, %d waiting, %d idle, %d used
	 Servers: %d active, %d idle, %d used
	 Client Wait Times: max %v, avg %v

 `,
		r.CCActive, r.CCWaiting, r.CCIdle, r.CCUsed,
		r.SCActive, r.SCIdle, r.SCUsed,
		time.Duration(r.CCMaxWait*1e9).Truncate(time.Millisecond),
		time.Duration(r.CCAvgWait*1e9).Truncate(time.Millisecond))
}

//------------------------------------------------------------------------------
// Pgpool

func pgpoolWriteHumanTo(fd io.Writer, o options, result *pgmetrics.Model) {
	fmt.Fprintf(fd, `
 pgmetrics run at: %s

 Pgpool Version:   %s
 `,
		fmtTimeAndSince(result.Metadata.At), result.Pgpool.Version,
	)

	// backends
	var tw tableWriter
	fmt.Fprintf(fd, `
 Pgpool Backends:
 `)
	tw.add("Node ID", "Host", "Port", "Status", "Role", "LB Weight", "Last Status Change")
	for _, b := range result.Pgpool.Backends {
		tw.add(b.NodeID, b.Hostname, b.Port, b.Status, b.Role, b.LBWeight,
			fmtTime(b.LastStatusChange))
	}
	tw.write(fd, "    ")

	// backend statement counts
	fmt.Fprintf(fd, `
 Pgpool Backend Statement Counts:
 `)
	tw.clear()
	tw.add("Node", "SELECT", "INSERT", "UPDATE", "DELETE", "DDL", "Other", "Panic", "Fatal", "Error")
	for _, b := range result.Pgpool.Backends {
		tw.add(fmt.Sprintf("%d (%s:%d)", b.NodeID, b.Hostname, b.Port),
			b.SelectCount, b.InsertCount, b.UpdateCount, b.DeleteCount,
			b.DDLCount, b.OtherCount, b.PanicCount, b.FatalCount, b.ErrorCount)
	}
	tw.write(fd, "    ")
}

//------------------------------------------------------------------------------
// helper functions

func fmtXIDRange(oldest, next int64) string {
	if oldest < 3 || oldest > math.MaxUint32 || next < 3 || next > math.MaxUint32 || oldest == next {
		return fmt.Sprintf("oldest = %d, next = %d (?)", oldest, next)
	}

	var r int64
	if oldest > next {
		r = (math.MaxUint32 - oldest + 1) + (next - 3)
	} else {
		r = next - oldest
	}

	return fmt.Sprintf("oldest = %d, next = %d, range = %d", oldest, next, r)
}

func fmtTime(at int64) string {
	if at == 0 {
		return ""
	}
	return time.Unix(at, 0).Format("2 Jan 2006 3:04:05 PM")
}

func fmtTimeAndSince(at int64) string {
	if at == 0 {
		return ""
	}
	t := time.Unix(at, 0)
	return fmt.Sprintf("%s (%s)", t.Format("2 Jan 2006 3:04:05 PM"),
		humanize.Time(t))
}

/* currently unused:
 func fmtTimeDef(at int64, def string) string {
	 if at == 0 {
		 return def
	 }
	 return time.Unix(at, 0).Format("2 Jan 2006 3:04:05 PM")
 }

 func fmtTimeAndSinceDef(at int64, def string) string {
	 if at == 0 {
		 return def
	 }
	 t := time.Unix(at, 0)
	 return fmt.Sprintf("%s (%s)", t.Format("2 Jan 2006 3:04:05 PM"),
		 humanize.Time(t))
 }

 func fmtSeconds(s string) string {
	 v, err := strconv.Atoi(s)
	 if err != nil {
		 return s
	 }
	 return (time.Duration(v) * time.Second).String()
 }
*/

func fmtSince(at int64) string {
	if at == 0 {
		return "never"
	}
	return humanize.Time(time.Unix(at, 0))
}

func fmtYesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func fmtYesBlank(v bool) string {
	if v {
		return "yes"
	}
	return ""
}

func fmtLag(a, b, qual string) string {
	if len(qual) > 0 && !strings.HasSuffix(qual, " ") {
		qual += " "
	}
	if d, ok := lsnDiff(a, b); ok {
		if d == 0 {
			return " (no " + qual + "lag)"
		}
		return fmt.Sprintf(" (%slag = %s)", qual, humanize.IBytes(uint64(d)))
	}
	return ""
}

func fmtIntZero(i int) string {
	if i == 0 {
		return ""
	}
	return strconv.Itoa(i)
}

func fmtPropagate(ins, upd, del bool) string {
	parts := make([]string, 0, 3)
	if ins {
		parts = append(parts, "inserts")
	}
	if upd {
		parts = append(parts, "updates")
	}
	if del {
		parts = append(parts, "deletes")
	}
	return strings.Join(parts, ", ")
}

func fmtMicros(v int64) string {
	s := (time.Duration(v) * time.Microsecond).String()
	return strings.Replace(s, "µ", "u", -1)
}

func getSetting(result *pgmetrics.Model, key string) string {
	if s, ok := result.Settings[key]; ok {
		return s.Setting
	}
	return ""
}

func getSettingInt(result *pgmetrics.Model, key string) int {
	s := getSetting(result, key)
	if len(s) == 0 {
		return 0
	}
	val, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return val
}

func getSettingBytes(result *pgmetrics.Model, key string, factor uint64) string {
	s := getSetting(result, key)
	if len(s) == 0 {
		return s
	}
	val, err := strconv.ParseUint(s, 10, 64)
	if err != nil || val == 0 {
		return s
	}
	return s + " (" + humanize.IBytes(val*factor) + ")"
}

func safeDiv(a, b int64) float64 {
	if b == 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func lsn2int(s string) int64 {
	if len(s) == 0 {
		return -1
	}
	if pos := strings.IndexByte(s, '/'); pos >= 0 {
		val1, err1 := strconv.ParseUint(s[:pos], 16, 64)
		val2, err2 := strconv.ParseUint(s[pos+1:], 16, 64)
		if err1 != nil || err2 != nil {
			return -1
		}
		return int64(val1<<32 | val2)
	}
	return -1
}

func lsnDiff(a, b string) (int64, bool) {
	va := lsn2int(a)
	vb := lsn2int(b)
	if va == -1 || vb == -1 {
		return -1, false
	}
	return va - vb, true
}

func getBlockSize(result *pgmetrics.Model) int {
	s := getSetting(result, "block_size")
	if len(s) == 0 {
		return 8192
	}
	v, err := strconv.Atoi(s)
	if err != nil || v == 0 {
		return 8192
	}
	return v
}

func getVersion(result *pgmetrics.Model) int {
	s := getSetting(result, "server_version_num")
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return v
}

func getMaxWalSize(result *pgmetrics.Model) (key, val string) {
	if version := getVersion(result); version >= pgv10 {
		key = "max_wal_size"
		val = getSettingBytes(result, key, 1024*1024)
	} else if version >= pgv95 {
		key = "max_wal_size"
		val = getSettingBytes(result, key, 16*1024*1024)
	} else {
		key = "checkpoint_segments"
		val = getSetting(result, key)
	}
	return
}

func getMinWalSize(result *pgmetrics.Model) (val string) {
	if version := getVersion(result); version >= pgv10 {
		val = getSettingBytes(result, "min_wal_size", 1024*1024)
	} else if version >= pgv95 {
		val = getSettingBytes(result, "min_wal_size", 16*1024*1024)
	}
	return
}

type aclItem struct {
	role    string
	privs   []string
	grantor string
}

var aclMap = map[byte]string{
	'a': "INSERT",
	'r': "SELECT",
	'w': "UPDATE",
	'd': "DELETE",
	'D': "TRUNCATE",
	'x': "REFERENCES",
	't': "TRIGGER",
	'X': "EXECUTE",
	'U': "USAGE",
	'C': "CREATE",
	'T': "TEMPORARY",
	'c': "CONNECT",
}

// see src/backend/utils/adt/acl.c
func parseACL(acl string) (out []aclItem) {
	for _, item := range strings.Split(acl, "\n") {
		if slash := strings.Split(item, "/"); len(slash) == 2 {
			e := aclItem{grantor: slash[1]}
			if eq := strings.Split(slash[0], "="); len(eq) == 2 {
				if eq[0] == "" {
					e.role = "PUBLIC"
				} else {
					e.role = eq[0]
				}
				for _, c := range eq[1] {
					if p, ok := aclMap[byte(c)]; ok {
						e.privs = append(e.privs, p)
					}
				}
				out = append(out, e)
			}
		}
	}
	return
}

//------------------------------------------------------------------------------

type tableWriter struct {
	data      [][]string
	hasFooter bool
}

const twLine = "\b1"

func (t *tableWriter) add(cols ...interface{}) {
	row := make([]string, len(cols))
	for i, c := range cols {
		row[i] = fmt.Sprintf("%v", c)
	}
	t.data = append(t.data, row)
}

func (t *tableWriter) clear() {
	t.data = nil
}

func (t *tableWriter) cols() int {
	n := 0
	for _, row := range t.data {
		if n < len(row) {
			n = len(row)
		}
	}
	return n
}

func (t *tableWriter) write(fd io.Writer, pfx string) (tw int) {
	if len(t.data) == 0 {
		return
	}
	ncols := t.cols()
	if ncols == 0 {
		return
	}
	// calculate widths
	widths := make([]int, ncols)
	for _, row := range t.data {
		for c, col := range row {
			w := len(col)
			if w > 1 && col[0] == '\b' {
				w = 0
			}
			if widths[c] < w {
				widths[c] = w
			}
		}
	}
	// calculate total width
	tw = len(pfx) + 1 // "prefix", "|"
	for _, w := range widths {
		tw += 1 + w + 1 + 1 // blank, "value", blank, "|"
	}
	// print line
	line := func() {
		fmt.Fprintf(fd, "%s+", pfx)
		for _, w := range widths {
			fmt.Fprint(fd, strings.Repeat("-", w+2))
			fmt.Fprintf(fd, "+")
		}
		fmt.Fprintln(fd)
	}
	line()
	for i, row := range t.data {
		if len(row) > 0 {
			if row[0] == twLine {
				line()
				continue
			}
		}
		if i == 1 || (t.hasFooter && i == len(t.data)-1) {
			line()
		}
		fmt.Fprintf(fd, "%s|", pfx)
		for c, col := range row {
			fmt.Fprintf(fd, " %*s |", widths[c], col)
		}
		fmt.Fprintln(fd)
	}
	line()
	return
}

// model2csv writes the CSV representation of the model into the given CSV writer.
func model2csv(m *pgmetrics.Model, w *csv.Writer) (err error) {
	defer func() {
		// return panic(error) as our error value
		if r := recover(); r != nil {
			if e, ok := r.(error); ok {
				err = e
			}
		}
	}()

	// meta data
	struct2csv("pgmetrics.meta.", m.Metadata, w)

	// top-level fields
	struct2csv("pgmetrics.", *m, w)

	// wal archiving
	struct2csv("pgmetrics.wal_archiving.", m.WALArchiving, w)

	// replication out
	rec2csv("pgmetrics.repl_out.count", strconv.Itoa(len(m.ReplicationOutgoing)), w)
	for i, rout := range m.ReplicationOutgoing {
		head := fmt.Sprintf("pgmetrics.repl_out.%d.", i)
		struct2csv(head, rout, w)
	}

	// replication in
	if m.ReplicationIncoming != nil {
		struct2csv("pgmetrics.repl_in.", *m.ReplicationIncoming, w)
	}

	// replication slots
	rec2csv("pgmetrics.repl_slot.count", strconv.Itoa(len(m.ReplicationSlots)), w)
	for _, rs := range m.ReplicationSlots {
		head := fmt.Sprintf("pgmetrics.repl_slot.%s.", rs.SlotName)
		struct2csv(head, rs, w)
	}

	// bgwriter
	struct2csv("pgmetrics.bg_writer.", m.BGWriter, w)

	// backends
	rec2csv("pgmetrics.backends.count", strconv.Itoa(len(m.Backends)), w)
	for i, be := range m.Backends {
		head := fmt.Sprintf("pgmetrics.backends.%d.", i)
		struct2csv(head, be, w)
	}

	// vacuum progress
	rec2csv("pgmetrics.vacuum.count", strconv.Itoa(len(m.VacuumProgress)), w)
	for i, vp := range m.VacuumProgress {
		head := fmt.Sprintf("pgmetrics.vacuum.%d.", i)
		struct2csv(head, vp, w)
	}

	// databases
	rec2csv("pgmetrics.databases.count", strconv.Itoa(len(m.Databases)), w)
	for _, db := range m.Databases {
		head := fmt.Sprintf("pgmetrics.databases.%s.", db.Name)
		struct2csv(head, db, w)
	}

	// tablespaces
	rec2csv("pgmetrics.tablespaces.count", strconv.Itoa(len(m.Tablespaces)), w)
	for _, ts := range m.Tablespaces {
		head := fmt.Sprintf("pgmetrics.tablespaces.%s.", ts.Name)
		struct2csv(head, ts, w)
	}

	// tables
	rec2csv("pgmetrics.tables.count", strconv.Itoa(len(m.Tables)), w)
	for _, t := range m.Tables {
		head := fmt.Sprintf("pgmetrics.tables.%s.%s.%s.", t.DBName, t.SchemaName, t.Name)
		struct2csv(head, t, w)
	}

	// indexes
	rec2csv("pgmetrics.indexes.count", strconv.Itoa(len(m.Indexes)), w)
	for _, idx := range m.Indexes {
		head := fmt.Sprintf("pgmetrics.indexes.%s.%s.%s.", idx.DBName, idx.SchemaName, idx.Name)
		struct2csv(head, idx, w)
	}

	// locks
	rec2csv("pgmetrics.locks.count", strconv.Itoa(len(m.Locks)), w)
	for i, l := range m.Locks {
		head := fmt.Sprintf("pgmetrics.locks.%d.", i)
		struct2csv(head, l, w)
	}

	// system metrics
	if m.System != nil {
		struct2csv("pgmetrics.system.", *(m.System), w)
	}

	// note: sequences, user functions, extensions, disabled triggers, statements,
	// roles, blocking pids, publications, subscriptions and settings are not
	// written to the csv as of now inorder to keep csv size small.
	return
}

func struct2csv(head string, s interface{}, w *csv.Writer) {
	t := reflect.TypeOf(s)
	if t.Kind() != reflect.Struct {
		panic(errors.New("struct2csv: arg is not a struct"))
	}
	v := reflect.ValueOf(s)
	for i := 0; i < t.NumField(); i++ {
		// make key
		f := t.Field(i)
		j := strings.Replace(f.Tag.Get("json"), ",omitempty", "", 1)
		if len(j) == 0 || j == "-" {
			continue
		}
		// make value
		var sv string
		switch fv := v.Field(i); fv.Kind() {
		case reflect.Int, reflect.Int64, reflect.Bool, reflect.Float64:
			sv = fmt.Sprintf("%v", fv)
		case reflect.String:
			sv = cleanstr(fv.String())
		}
		if len(sv) > 0 {
			// write key, value into csv writer
			rec2csv(head+j, sv, w)
		}
	}
}

func rec2csv(key, value string, w *csv.Writer) {
	if err := w.Write([]string{key, value}); err != nil {
		panic(err)
	}
}

var cleanrepl = strings.NewReplacer("\t", " ", "\r", " ", "\n", " ")

func cleanstr(s string) string {
	s = cleanrepl.Replace(s)
	if len(s) > 1024 {
		s = s[:1024]
	}
	return s
}

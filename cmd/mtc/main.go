package main

import (
	"github.com/bwesterb/mtc"
	"github.com/bwesterb/mtc/ca"

	"github.com/urfave/cli/v2"
	"golang.org/x/crypto/cryptobyte"

	"bufio"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime/pprof"
	"text/tabwriter"
	"time"
)

var (
	errNoCaParams = errors.New("missing ca-params flag")
	errArgs       = errors.New("Wrong number of arguments")
	fCpuProfile   *os.File
)

// Writes buf either to stdout (if path is empty) or path.
func writeToFileOrStdout(path string, buf []byte) error {
	if path != "" {
		err := os.WriteFile(path, buf, 0644)
		if err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
		return nil
	}

	_, err := os.Stdout.Write(buf)
	if err != nil {
		return fmt.Errorf("writing to stdout: %w", err)
	}

	return nil
}

// Flags used to create or specify an assertion. Used in `mtc ca queue'.
// Includes the in-file flag, if inFile is true.
func assertionFlags(inFile bool) []cli.Flag {
	ret := []cli.Flag{
		&cli.StringSliceFlag{
			Name:     "dns",
			Aliases:  []string{"d"},
			Category: "Assertion",
		},
		&cli.StringSliceFlag{
			Name:     "dns-wildcard",
			Aliases:  []string{"w"},
			Category: "Assertion",
		},
		&cli.StringSliceFlag{
			Name:     "ens",
			Aliases:  []string{"e"},
			Category: "Assertion",
		},
		&cli.StringSliceFlag{
			Name:     "ip4",
			Category: "Assertion",
		},
		&cli.StringSliceFlag{
			Name:     "ip6",
			Category: "Assertion",
		},

		&cli.StringFlag{
			Name:     "tls-pem",
			Category: "Assertion",
			Usage:    "path to PEM encoded subject public key",
		},
		&cli.StringFlag{
			Name:     "tls-der",
			Category: "Assertion",
			Usage:    "path to DER encoded subject public key",
		},
		&cli.StringFlag{
			Name:     "tls-scheme",
			Category: "Assertion",
			Usage:    "TLS signature scheme to be used by subject",
		},
		&cli.StringFlag{
			Name:     "checksum",
			Category: "Assertion",
			Usage:    "Only proceed if assertion matches checksum",
		},
	}
	if inFile {
		ret = append(
			ret,
			&cli.StringFlag{
				Name:     "in-file",
				Category: "Assertion",
				Aliases:  []string{"i"},
				Usage:    "Read assertion from the given file",
			},
		)
	}

	return ret
}

func assertionFromFlags(cc *cli.Context) (*ca.QueuedAssertion, error) {
	qa, err := assertionFromFlagsUnchecked(cc)
	if err != nil {
		return nil, err
	}

	err = qa.Check()
	if err != nil {
		return nil, err
	}

	return qa, nil
}

func assertionFromFlagsUnchecked(cc *cli.Context) (*ca.QueuedAssertion, error) {
	var (
		checksum []byte
		err      error
	)

	if cc.String("checksum") != "" {
		checksum, err = hex.DecodeString(cc.String("checksum"))
		if err != nil {
			fmt.Errorf("Parsing checksum: %w", err)
		}
	}

	assertionPath := cc.String("in-file")
	if assertionPath != "" {
		assertionBuf, err := os.ReadFile(assertionPath)
		if err != nil {
			return nil, fmt.Errorf(
				"reading assertion %s: %w",
				assertionPath,
				err,
			)
		}

		for _, flag := range []string{
			"dns",
			"dns-wildcard",
			"ens",
			"ip4",
			"ip6",
			"tls-der",
			"tls-pem",
		} {
			if cc.IsSet(flag) {
				return nil, fmt.Errorf(
					"Can't specify --in-file and --%s together",
					flag,
				)
			}
		}

		var a mtc.Assertion
		err = a.UnmarshalBinary(assertionBuf)
		if err != nil {
			return nil, fmt.Errorf(
				"parsing assertion %s: %w",
				assertionPath,
				err,
			)
		}

		return &ca.QueuedAssertion{
			Assertion: a,
			Checksum:  checksum,
		}, nil
	}

	cs := mtc.Claims{
		DNS:         cc.StringSlice("dns"),
		DNSWildcard: cc.StringSlice("dns-wildcard"),
	}

        cs.ENS = cc.StringSlice("ens")
	for _, ip := range cc.StringSlice("ip4") {
		cs.IPv4 = append(cs.IPv4, net.ParseIP(ip))
	}

	for _, ip := range cc.StringSlice("ip6") {
		cs.IPv6 = append(cs.IPv6, net.ParseIP(ip))
	}

	if (cc.String("tls-pem") == "" &&
		cc.String("tls-der") == "") ||
		(cc.String("tls-pem") != "" &&
			cc.String("tls-der") != "") {
		return nil, errors.New("Expect either tls-pem or tls-der flag")
	}

	usingPem := false
	subjectPath := cc.String("tls-der")
	if cc.String("tls-pem") != "" {
		usingPem = true
		subjectPath = cc.String("tls-pem")
	}

	subjectBuf, err := os.ReadFile(subjectPath)
	if err != nil {
		return nil, fmt.Errorf("reading subject %s: %w", subjectPath, err)
	}

	if usingPem {
		block, _ := pem.Decode([]byte(subjectBuf))
		if block == nil {
			return nil, fmt.Errorf(
				"reading subject %s: failed to parse PEM block",
				subjectPath,
			)
		}
		subjectBuf = block.Bytes
	}

	pub, err := x509.ParsePKIXPublicKey(subjectBuf)
	if err != nil {
		return nil, fmt.Errorf("Parsing subject %s: %w", subjectPath, err)
	}

	var scheme mtc.SignatureScheme
	if cc.String("tls-scheme") != "" {
		scheme = mtc.SignatureSchemeFromString(cc.String("tls-scheme"))
		if scheme == 0 {
			return nil, fmt.Errorf("Unknown TLS signature scheme: %s", scheme)
		}
	} else {
		schemes := mtc.SignatureSchemesFor(pub)
		if len(schemes) == 0 {
			return nil, fmt.Errorf(
				"No matching signature scheme for that public key",
			)
		}
		if len(schemes) >= 2 {
			return nil, fmt.Errorf(
				"Specify --tls-scheme with one of %s",
				schemes,
			)
		}
		scheme = schemes[0]
	}

	subj, err := mtc.NewTLSSubject(scheme, pub)
	if err != nil {
		return nil, fmt.Errorf("creating subject: %w", err)
	}

	a := mtc.Assertion{
		Claims:  cs,
		Subject: subj,
	}

	return &ca.QueuedAssertion{
		Assertion: a,
		Checksum:  checksum,
	}, nil
}

func handleCaQueue(cc *cli.Context) error {
	qa, err := assertionFromFlags(cc)
	if err != nil {
		return err
	}

	h, err := ca.Open(cc.String("ca-path"))
	if err != nil {
		return err
	}
	defer h.Close()

	return h.QueueMultiple(func(yield func(qa ca.QueuedAssertion) error) error {
		for i := 0; i < cc.Int("debug-repeat"); i++ {
			qa2 := *qa
			if cc.Bool("debug-vary") {
				qa2.Checksum = nil
				qa2.Assertion.Claims.DNS = append(
					qa2.Assertion.Claims.DNS,
					fmt.Sprintf("%d.example.com", i),
				)
			}
			if err := yield(qa2); err != nil {
				return err
			}
		}
		return nil
	})
}

func handleNewAssertion(cc *cli.Context) error {
	qa, err := assertionFromFlags(cc)
	if err != nil {
		return err
	}

	buf, err := qa.Assertion.MarshalBinary()
	if err != nil {
		return err
	}

	if err := writeToFileOrStdout(cc.String("out-file"), buf); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "checksum: %x\n", qa.Checksum)

	return nil
}

func handleCaIssue(cc *cli.Context) error {
	h, err := ca.Open(cc.String("ca-path"))
	if err != nil {
		return err
	}
	defer h.Close()

	return h.Issue()
}

func handleCaCert(cc *cli.Context) error {
	h, err := ca.Open(cc.String("ca-path"))
	if err != nil {
		return err
	}
	defer h.Close()

	qa, err := assertionFromFlags(cc)
	if err != nil {
		return err
	}

	cert, err := h.CertificateFor(qa.Assertion)
	if err != nil {
		return err
	}

	buf, err := cert.MarshalBinary()
	if err != nil {
		return err
	}

	if err := writeToFileOrStdout(cc.String("out-file"), buf); err != nil {
		return err
	}

	return nil
}

func handleCaShowQueue(cc *cli.Context) error {
	h, err := ca.Open(cc.String("ca-path"))
	if err != nil {
		return err
	}
	defer h.Close()

	count := 0

	err = h.WalkQueue(func(qa ca.QueuedAssertion) error {
		count++
		a := qa.Assertion
		cs := a.Claims
		subj := a.Subject
		w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
		fmt.Fprintf(w, "checksum\t%x\n", qa.Checksum)
		fmt.Fprintf(w, "subject_type\t%s\n", subj.Type())
		switch subj := subj.(type) {
		case *mtc.TLSSubject:
			asubj := subj.Abridge().(*mtc.AbridgedTLSSubject)
			fmt.Fprintf(w, "signature_scheme\t%s\n", asubj.SignatureScheme)
			fmt.Fprintf(w, "public_key_hash\t%x\n", asubj.PublicKeyHash[:])
		}
		if len(cs.DNS) != 0 {
			fmt.Fprintf(w, "dns\t%s\n", cs.DNS)
		}
		if len(cs.ENS) != 0 {
			fmt.Fprintf(w, "ens\t%s\n", cs.ENS)
		}
		if len(cs.DNSWildcard) != 0 {
			fmt.Fprintf(w, "dns_wildcard\t%s\n", cs.DNSWildcard)
		}
		if len(cs.IPv4) != 0 {
			fmt.Fprintf(w, "ip4\t%s\n", cs.IPv4)
		}
		if len(cs.IPv6) != 0 {
			fmt.Fprintf(w, "ip6\t%s\n", cs.IPv6)
		}
		w.Flush()
		fmt.Printf("\n")
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Printf("Total number of assertions in queue: %d\n", count)
	return nil
}

func handleCaNew(cc *cli.Context) error {
	if cc.Args().Len() != 2 {
		cli.ShowSubcommandHelp(cc)
		return errArgs
	}
	h, err := ca.New(
		cc.String("ca-path"),
		ca.NewOpts{
			IssuerId:   cc.Args().Get(0),
			HttpServer: cc.Args().Get(1),

			BatchDuration:   cc.Duration("batch-duration"),
			StorageDuration: cc.Duration("storage-duration"),
			Lifetime:        cc.Duration("lifetime"),
		},
	)
	if err != nil {
		return err
	}
	h.Close()
	return nil
}

// Get the data at hand to inspect for an inspect subcommand, by either
// reading it from stdin or a file
func inspectGetBuf(cc *cli.Context) ([]byte, error) {
	r, err := inspectGetReader(cc)
	if err != nil {
		return nil, err
	}
	buf, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	err = r.Close()
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// Same as inspectGetBuf(), but returns a io.ReadCloser instead.
func inspectGetReader(cc *cli.Context) (io.ReadCloser, error) {
	if cc.Args().Len() == 0 {
		return os.Stdin, nil
	}
	r, err := os.Open(cc.Args().Get(0))
	if err != nil {
		return nil, err
	}
	return r, nil
}

func inspectGetCAParams(cc *cli.Context) (*mtc.CAParams, error) {
	var p mtc.CAParams
	path := cc.String("ca-params")
	if path == "" {
		return nil, errNoCaParams
	}
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := p.UnmarshalBinary(buf); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &p, nil
}

func handleInspectSignedValidityWindow(cc *cli.Context) error {
	buf, err := inspectGetBuf(cc)
	if err != nil {
		return err
	}
	p, err := inspectGetCAParams(cc)
	if err != nil {
		return err
	}

	var sw mtc.SignedValidityWindow
	err = sw.UnmarshalBinary(buf, p) // this also checks the signature
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintf(w, "signature\t✅\n")
	fmt.Fprintf(w, "batch_number\t%d\n", sw.ValidityWindow.BatchNumber)
	for i := 0; i < int(p.ValidityWindowSize); i++ {
		fmt.Fprintf(
			w,
			"tree_heads[%d]\t%x\n",
			int(sw.ValidityWindow.BatchNumber)+i-int(p.ValidityWindowSize)+1,
			sw.ValidityWindow.TreeHeads[mtc.HashLen*i:mtc.HashLen*(i+1)],
		)
	}

	w.Flush()
	return nil
}

func handleInspectIndex(cc *cli.Context) error {
	buf, err := inspectGetBuf(cc)
	if err != nil {
		return err
	}

	var (
		key    []byte
		seqno  uint64
		offset uint64
	)

	s := cryptobyte.String(buf)

	total := 0
	fmt.Printf("%64s %7s %7s\n", "key", "seqno", "offset")
	for !s.Empty() {
		if !s.ReadBytes(&key, 32) || !s.ReadUint64(&seqno) || !s.ReadUint64(&offset) {
			return errors.New("truncated")
		}

		fmt.Printf("%x %7d %7d\n", key, seqno, offset)
		total++
	}

	fmt.Printf("\ntotal number of entries: %d\n", total)

	return nil
}

func handleInspectTree(cc *cli.Context) error {
	buf, err := inspectGetBuf(cc)
	if err != nil {
		return err
	}

	var t mtc.Tree
	err = t.UnmarshalBinary(buf)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintf(w, "number of leaves\t%d\n", t.LeafCount())
	fmt.Fprintf(w, "number of nodes\t%d\n", t.NodeCount())
	fmt.Fprintf(w, "root\t%x\n", t.Root())
	w.Flush()
	return nil
}

func writeAssertion(w *tabwriter.Writer, a mtc.Assertion) {
	aa := a.Abridge()
	cs := aa.Claims
	subj := aa.Subject
	fmt.Fprintf(w, "subject_type\t%s\n", subj.Type())
	switch subj := subj.(type) {
	case *mtc.AbridgedTLSSubject:
		fmt.Fprintf(w, "signature_scheme\t%s\n", subj.SignatureScheme)
		fmt.Fprintf(w, "public_key_hash\t%x\n", subj.PublicKeyHash[:])
	}
	if len(cs.DNS) != 0 {
		fmt.Fprintf(w, "dns\t%s\n", cs.DNS)
	}
	if len(cs.DNSWildcard) != 0 {
		fmt.Fprintf(w, "dns_wildcard\t%s\n", cs.DNSWildcard)
	}
	if len(cs.ENS) != 0 {
		fmt.Fprintf(w, "ens\t%s\n", cs.ENS)
	}
	if len(cs.IPv4) != 0 {
		fmt.Fprintf(w, "ip4\t%s\n", cs.IPv4)
	}
	if len(cs.IPv6) != 0 {
		fmt.Fprintf(w, "ip6\t%s\n", cs.IPv6)
	}
}

func handleInspectCert(cc *cli.Context) error {
	buf, err := inspectGetBuf(cc)
	if err != nil {
		return err
	}

	var c mtc.BikeshedCertificate
	err = c.UnmarshalBinary(buf)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	writeAssertion(w, c.Assertion)
	fmt.Fprintf(w, "\n")
	fmt.Fprintf(w, "proof_type\t%v\n", c.Proof.TrustAnchor().ProofType())

	switch anch := c.Proof.TrustAnchor().(type) {
	case *mtc.MerkleTreeTrustAnchor:
		fmt.Fprintf(w, "issuer_id\t%s\n", anch.IssuerId())
		fmt.Fprintf(w, "batch\t%d\n", anch.BatchNumber())
	}

	switch proof := c.Proof.(type) {
	case *mtc.MerkleTreeProof:
		fmt.Fprintf(w, "index\t%d\n", proof.Index())
	}

	switch proof := c.Proof.(type) {
	case *mtc.MerkleTreeProof:
		path := proof.Path()

		params, err := inspectGetCAParams(cc)
		if err == nil {
			anch := proof.TrustAnchor().(*mtc.MerkleTreeTrustAnchor)

			batch := &mtc.Batch{
				CA:     params,
				Number: anch.BatchNumber(),
			}

			if anch.IssuerId() != params.IssuerId {
				return fmt.Errorf(
					"IssuerId doesn't match: %s ≠ %s",
					params.IssuerId,
					anch.IssuerId(),
				)
			}
			aa := c.Assertion.Abridge()
			root, err := batch.ComputeRootFromAuthenticationPath(
				proof.Index(),
				path,
				&aa,
			)
			if err != nil {
				return fmt.Errorf("computing root: %w", err)
			}

			fmt.Fprintf(w, "recomputed root\t%x\n", root)
		} else if err != errNoCaParams {
			return err
		}

		w.Flush()
		fmt.Printf("authentication path\n")
		for i := 0; i < len(path)/mtc.HashLen; i++ {
			fmt.Printf(" %x\n", path[i*mtc.HashLen:(i+1)*mtc.HashLen])
		}
	}

	w.Flush()
	return nil
}

func handleInspectAssertion(cc *cli.Context) error {
	buf, err := inspectGetBuf(cc)
	if err != nil {
		return err
	}

	var a mtc.Assertion
	err = a.UnmarshalBinary(buf)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	writeAssertion(w, a)
	w.Flush()
	return nil
}

func handleInspectAbridgedAssertions(cc *cli.Context) error {
	r, err := inspectGetReader(cc)
	if err != nil {
		return err
	}
	defer r.Close()

	count := 0
	err = mtc.UnmarshalAbridgedAssertions(
		bufio.NewReader(r),
		func(_ int, aa *mtc.AbridgedAssertion) error {
			count++
			cs := aa.Claims
			subj := aa.Subject
			var key [mtc.HashLen]byte
			aa.Key(key[:])
			w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
			fmt.Fprintf(w, "key\t%x\n", key)
			fmt.Fprintf(w, "subject_type\t%s\n", subj.Type())
			switch subj := subj.(type) {
			case *mtc.AbridgedTLSSubject:
				fmt.Fprintf(w, "signature_scheme\t%s\n", subj.SignatureScheme)
				fmt.Fprintf(w, "public_key_hash\t%x\n", subj.PublicKeyHash[:])
			}
			if len(cs.DNS) != 0 {
				fmt.Fprintf(w, "dns\t%s\n", cs.DNS)
			}
			if len(cs.ENS) != 0 {
				fmt.Fprintf(w, "ens\t%s\n", cs.ENS)
			}
			if len(cs.DNSWildcard) != 0 {
				fmt.Fprintf(w, "dns_wildcard\t%s\n", cs.DNSWildcard)
			}
			if len(cs.IPv4) != 0 {
				fmt.Fprintf(w, "ip4\t%s\n", cs.IPv4)
			}
			if len(cs.IPv6) != 0 {
				fmt.Fprintf(w, "ip6\t%s\n", cs.IPv6)
			}
			w.Flush()
			fmt.Printf("\n")
			return nil
		},
	)
	if err != nil {
		return err
	}
	fmt.Printf("Total number of abridged assertions: %d\n", count)
	return nil
}

func handleInspectCaParams(cc *cli.Context) error {
	buf, err := inspectGetBuf(cc)
	if err != nil {
		return err
	}
	var p mtc.CAParams
	err = p.UnmarshalBinary(buf)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 1, 1, 1, ' ', 0)
	fmt.Fprintf(w, "issuer_id\t%s\n", p.IssuerId)
	fmt.Fprintf(w, "start_time\t%d\t%s\n", p.StartTime,
		time.Unix(int64(p.StartTime), 0))
	fmt.Fprintf(w, "batch_duration\t%d\t%s\n", p.BatchDuration,
		time.Second*time.Duration(p.BatchDuration))
	fmt.Fprintf(w, "life_time\t%d\t%s\n", p.Lifetime,
		time.Second*time.Duration(p.Lifetime))
	fmt.Fprintf(w, "storage_window_size\t%d\t%s\n", p.StorageWindowSize,
		time.Second*time.Duration(p.BatchDuration*p.StorageWindowSize))
	fmt.Fprintf(w, "validity_window_size\t%d\n", p.ValidityWindowSize)
	fmt.Fprintf(w, "http_server\t%s\n", p.HttpServer)
	fmt.Fprintf(
		w,
		"public_key fingerprint\t%s\n",
		mtc.VerifierFingerprint(p.PublicKey),
	)
	w.Flush()
	return nil
}

func main() {
	app := &cli.App{
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "cpuprofile",
				Usage: "write cpu profile to file",
			},
		},
		Commands: []*cli.Command{
			{
				Name: "ca",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:  "ca-path",
						Usage: "path to CA state",
						Value: ".",
					},
				},
				Subcommands: []*cli.Command{
					{
						Name:      "new",
						Usage:     "creates a new CA",
						Action:    handleCaNew,
						ArgsUsage: "<issuer-id> <http-server>",
						Flags: []cli.Flag{
							&cli.DurationFlag{
								Name:    "batch-duration",
								Aliases: []string{"b"},
								Usage:   "time between batches",
							},
							&cli.DurationFlag{
								Name:    "lifetime",
								Aliases: []string{"l"},
								Usage:   "lifetime of an assertion",
							},
							&cli.DurationFlag{
								Name:    "storage-duration",
								Aliases: []string{"s"},
								Usage:   "time to serve assertions",
							},
						},
					},
					{
						Name:   "show-queue",
						Usage:  "prints the queue",
						Action: handleCaShowQueue,
					},
					{
						Name:   "issue",
						Usage:  "certify and issue queued assertions",
						Action: handleCaIssue,
					},
					{
						Name:   "queue",
						Usage:  "queue assertion for issuance",
						Action: handleCaQueue,
						Flags: append(
							assertionFlags(true),
							&cli.IntFlag{
								Name:     "debug-repeat",
								Category: "Debug",
								Usage:    "Queue the same assertion several times",
								Value:    1,
							},
							&cli.BoolFlag{
								Name:     "debug-vary",
								Category: "Debug",
								Usage:    "Varies each repeated assertion slightly",
							},
						),
					},
					{
						Name:   "cert",
						Usage:  "creates certificate for an issued assertion",
						Action: handleCaCert,
						Flags: append(
							assertionFlags(true),
							&cli.StringFlag{
								Name:    "out-file",
								Usage:   "path to write assertion to",
								Aliases: []string{"o"},
							},
						),
					},
				},
			},
			{
				Name: "inspect",
				Subcommands: []*cli.Command{
					{
						Name:      "ca-params",
						Usage:     "parses ca-params file",
						Action:    handleInspectCaParams,
						ArgsUsage: "[path]",
					},
					{
						Name:      "signed-validity-window",
						Usage:     "parses batch's signed-validity-window file",
						Action:    handleInspectSignedValidityWindow,
						ArgsUsage: "[path]",
					},
					{
						Name:      "abridged-assertions",
						Usage:     "parses batch's abridged-assertions file",
						Action:    handleInspectAbridgedAssertions,
						ArgsUsage: "[path]",
					},
					{
						Name:      "assertion",
						Usage:     "parses an assertion",
						Action:    handleInspectAssertion,
						ArgsUsage: "[path]",
					},
					{
						Name:      "tree",
						Usage:     "parses batch's tree file",
						Action:    handleInspectTree,
						ArgsUsage: "[path]",
					},
					{
						Name:      "index",
						Usage:     "parses batch's index file",
						Action:    handleInspectIndex,
						ArgsUsage: "[path]",
					},
					{
						Name:      "cert",
						Usage:     "parses a certificate",
						Action:    handleInspectCert,
						ArgsUsage: "[path]",
					},
				},
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name:    "ca-params",
						Usage:   "path to CA parameters required to parse some files",
						Aliases: []string{"p"},
					},
				},
			},
			{
				Name:   "new-assertion",
				Usage:  "creates a new assertion",
				Action: handleNewAssertion,
				Flags: append(
					assertionFlags(false),
					&cli.StringFlag{
						Name:    "out-file",
						Usage:   "path to write assertion to",
						Aliases: []string{"o"},
					},
				),
			},
		},
		Before: func(cc *cli.Context) error {
			if path := cc.String("cpuprofile"); path != "" {
				var err error
				fCpuProfile, err = os.Create(path)
				if err != nil {
					return fmt.Errorf("create(%s): %w", path, err)
				}
				pprof.StartCPUProfile(fCpuProfile)
			}
			return nil
		},
		After: func(cc *cli.Context) error {
			if fCpuProfile != nil {
				pprof.StopCPUProfile()
			}
			return nil
		},
	}

	if err := app.Run(os.Args); err != nil {
		if err != errArgs {
			fmt.Printf("error: %v\n", err.Error())
		}
		os.Exit(1)
	}
}

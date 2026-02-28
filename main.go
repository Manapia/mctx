package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/newmo-oss/ergo"
	"github.com/spf13/cobra"
)

var version string

type BundleCmdOption struct {
	ExcludePatterns []string
	IncludeHidden   bool
	Verbose         bool
}

func main() {
	if err := NewCmd().Execute(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
	}
}

func NewCmd() *cobra.Command {
	option := BundleCmdOption{}

	cmd := &cobra.Command{
		Use:           "mctx <input_path>... <output>",
		Short:         "Bundle multiple text files into a single context file",
		SilenceErrors: true,
		SilenceUsage:  true,
		Version:       version,
		Args:          cobra.MinimumNArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if err := runBundleCmd(cmd, args, option); err != nil {
				_, _ = fmt.Fprintln(os.Stderr, err)
				for attr := range ergo.AttrsAll(err) {
					_, _ = fmt.Fprintln(os.Stderr, attr.String())
				}
				if option.Verbose {
					for _, frame := range ergo.StackTraceOf(err) {
						_, _ = fmt.Fprintf(os.Stderr, "%s/%s:%d\n", frame.PkgPath(), frame.File(), frame.Line())
					}
				}
			}
		},
	}

	f := cmd.Flags()

	f.StringSliceVarP(&option.ExcludePatterns, "exclude", "e", []string{}, "Exclude files matching the given pattern")
	f.BoolVarP(&option.IncludeHidden, "hidden", "H", false, "Include hidden files (files or directories starting with a dot)")
	f.BoolVarP(&option.Verbose, "verbose", "", false, "Enable verbose output")

	return cmd
}

func runBundleCmd(cmd *cobra.Command, args []string, option BundleCmdOption) error {
	var srcFilePathList []string

	for i, pattern := range option.ExcludePatterns {
		option.ExcludePatterns[i] = filepath.ToSlash(pattern)
	}

	patterns := args[:len(args)-1]
	for _, pattern := range patterns {
		paths, err := doublestar.FilepathGlob(filepath.ToSlash(pattern))
		if err != nil {
			return ergo.Wrap(err, "matching files", slog.String("pattern", pattern))
		}

	pathLoop:
		for _, matchedPath := range paths {
			cleanPath := filepath.ToSlash(matchedPath)
			for _, exp := range option.ExcludePatterns {
				match, err := doublestar.PathMatch(exp, cleanPath)
				if err != nil {
					return ergo.Wrap(err, "match exclude pattern", slog.String("pattern", exp))
				}
				if match {
					continue pathLoop
				}
			}

			if !option.IncludeHidden {
				for _, seg := range strings.Split(cleanPath, "/") {
					if strings.HasPrefix(seg, ".") {
						continue pathLoop
					}
				}
			}

			stat, err := os.Stat(matchedPath)
			if err != nil {
				return ergo.Wrap(err, "get file info")
			} else if stat.IsDir() {
				continue
			}
			srcFilePathList = append(srcFilePathList, matchedPath)
		}
	}

	if len(srcFilePathList) == 0 {
		return nil
	}

	outputPath := args[len(args)-1]
	var outputFile io.Writer
	outputIsStdOut := outputPath == "-"
	if outputPath == "-" {
		outputFile = os.Stdout
	} else {
		file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
		if err != nil {
			return ergo.Wrap(err, "open output file")
		}
		defer func() { _ = file.Close() }()
		outputFile = file
	}

	for _, srcFilePath := range srcFilePathList {
		if err := writeToFile(srcFilePath, outputFile); err != nil {
			return ergo.Wrap(err, "write to file", slog.String("src", srcFilePath), slog.String("dst", outputPath))
		}

		if !outputIsStdOut {
			_, _ = fmt.Fprintf(os.Stdout, "Bundled: %s\n", srcFilePath)
		}
	}

	return nil
}

func writeToFile(srcFilePath string, dst io.Writer) error {
	srcFile, err := os.Open(srcFilePath)
	if err != nil {
		return ergo.Wrap(err, "open source file")
	}
	defer func() { _ = srcFile.Close() }()

	// ファイルの最初を読み込んで NULL が含まれていたらバイナリファイルとして認識する
	buf := make([]byte, 512)
	n, err := srcFile.Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		return ergo.Wrap(err, "read source file head")
	}
	buf = buf[:n]
	isBinaryFile := bytes.IndexByte(buf, 0x00) != -1

	startDelim := ">>>> START: " + srcFilePath + " <<<<\n"
	if _, err := io.WriteString(dst, startDelim); err != nil {
		return ergo.Wrap(err, "write a start delimiter")
	}

	if !isBinaryFile {
		// 判定のために読み込んだバッファ(先頭最大512byte)を先に書き込んで残りをコピー
		if _, err := dst.Write(buf); err != nil {
			return ergo.Wrap(err, "write file head data")
		}
		if _, err := io.Copy(dst, srcFile); err != nil {
			return ergo.Wrap(err, "copy file data")
		}
	} else {
		if _, err := io.WriteString(dst, "[Binary file: content skipped]"); err != nil {
			return ergo.Wrap(err, "write binary file skip message")
		}
	}

	endDelim := "\n>>>> END: " + srcFilePath + " <<<<\n\n"
	if _, err := io.WriteString(dst, endDelim); err != nil {
		return ergo.Wrap(err, "write an end delimiter")
	}

	return nil
}

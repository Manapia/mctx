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
	"github.com/spf13/pflag"
)

var version string

type BundleCmdOption struct {
	ExcludePatterns []string
	Includes        []string
	IncludeHidden   bool
	WriteIndex      bool
	ListOnly        bool
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
				printError(err, option.Verbose)
			}
		},
	}

	bindCmdOptionFlags(&option, cmd.Flags())

	cmd.AddCommand(NewGitCmd())

	return cmd
}

func printError(err error, verbose bool) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	for attr := range ergo.AttrsAll(err) {
		_, _ = fmt.Fprintln(os.Stderr, attr.String())
	}
	if verbose {
		for _, frame := range ergo.StackTraceOf(err) {
			_, _ = fmt.Fprintf(os.Stderr, "%s/%s:%d\n", frame.PkgPath(), frame.File(), frame.Line())
		}
	}
}

func bindCmdOptionFlags(option *BundleCmdOption, f *pflag.FlagSet) {
	f.StringSliceVarP(&option.ExcludePatterns, "exclude", "e", []string{}, "Exclude files matching the given pattern")
	f.StringSliceVarP(&option.Includes, "include", "i", []string{}, "Include files matching the given pattern")
	f.BoolVarP(&option.IncludeHidden, "hidden", "H", false, "Include hidden files (files or directories starting with a dot)")
	f.BoolVarP(&option.WriteIndex, "index", "I", false, "Write an index of bundled files")
	f.BoolVarP(&option.ListOnly, "list", "l", false, "List target files without bundling")
	f.BoolVarP(&option.Verbose, "verbose", "", false, "Enable verbose output")
}

func runBundleCmd(cmd *cobra.Command, args []string, option BundleCmdOption) error {
	var srcFilePathList []string

	for i, pattern := range option.ExcludePatterns {
		option.ExcludePatterns[i] = filepath.ToSlash(pattern)
	}

	filterOption := FilterOption{
		ExcludePatterns: option.ExcludePatterns,
		Includes:        option.Includes,
		IncludeHidden:   option.IncludeHidden,
	}

	patterns := args[:len(args)-1]
	for _, pattern := range patterns {
		paths, err := doublestar.FilepathGlob(filepath.ToSlash(pattern))
		if err != nil {
			return ergo.Wrap(err, "matching files", slog.String("pattern", pattern))
		}

		for _, matchedPath := range paths {
			filtered, err := filterFilePath("", matchedPath, filterOption)
			if err != nil {
				return ergo.Wrap(err, "filter matched file path", slog.String("filePath", matchedPath))
			}
			if filtered {
				srcFilePathList = append(srcFilePathList, matchedPath)
			}
		}
	}

	if len(srcFilePathList) == 0 {
		return nil
	}

	if option.ListOnly {
		for _, s := range srcFilePathList {
			fmt.Println(s)
		}
		return nil
	}

	outputPath := args[len(args)-1]
	outputFile, err := prepareOutput(outputPath)
	if err != nil {
		return ergo.Wrap(err, "prepare output")
	}

	if option.WriteIndex {
		if err := writeIndexOfFiles(srcFilePathList, outputFile); err != nil {
			return ergo.Wrap(err, "write index of files")
		}
	}

	for _, srcFilePath := range srcFilePathList {
		if err := writeToFile(srcFilePath, outputFile); err != nil {
			return ergo.Wrap(err, "write to file", slog.String("src", srcFilePath), slog.String("dst", outputPath))
		}
	}

	return nil
}

func prepareOutput(outputPath string) (io.Writer, error) {
	var outputFile io.Writer
	if outputPath == "-" {
		outputFile = os.Stdout
	} else {
		file, err := os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
		if err != nil {
			return nil, ergo.Wrap(err, "open output file")
		}
		defer func() { _ = file.Close() }()
		outputFile = file
	}

	return outputFile, nil
}

type FilterOption struct {
	ExcludePatterns []string
	Includes        []string
	IncludeHidden   bool
}

func filterFilePath(basePath, relPath string, option FilterOption) (bool, error) {
	cleanRelPath := filepath.ToSlash(relPath)

	for _, exp := range option.ExcludePatterns {
		match, err := doublestar.PathMatch(exp, cleanRelPath)
		if err != nil {
			return false, ergo.Wrap(err, "match exclude pattern", slog.String("pattern", exp))
		}
		if match {
			return false, nil
		}
	}

	if !option.IncludeHidden {
		for _, seg := range strings.Split(cleanRelPath, "/") {
			// カレントディレクトリを表す . や .. を隠しファイル扱いしない
			if seg == "." || seg == ".." {
				continue
			}
			if strings.HasPrefix(seg, ".") {
				return false, nil
			}
		}
	}

	fullPath := relPath
	if basePath != "" {
		fullPath = filepath.Join(basePath, relPath)
	}

	if isDir, err := checkIsDirectory(fullPath); err != nil {
		return false, ergo.Wrap(err, "")
	} else if isDir {
		return false, nil
	}

	return true, nil
}

func checkIsDirectory(filePath string) (bool, error) {
	stat, err := os.Stat(filePath)
	if err != nil {
		return false, ergo.Wrap(err, "get file info")
	}
	return stat.IsDir(), nil
}

func writeIndexOfFiles(filePaths []string, output io.Writer) error {
	sb := strings.Builder{}

	sb.WriteString("========= FILE INDEX =========\n")

	for _, filePath := range filePaths {
		sb.WriteString(filePath)
		sb.WriteByte('\n')
	}

	sb.WriteString("==============================\n\n")

	if _, err := io.WriteString(output, sb.String()); err != nil {
		return ergo.Wrap(err, "write index of files to output")
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

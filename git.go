package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/newmo-oss/ergo"
	"github.com/spf13/cobra"
)

type GitCmdOption struct {
	BundleCmdOption
	SkipConflicts  bool
	IncludeIgnored bool
}

func NewGitCmd() *cobra.Command {
	option := GitCmdOption{}

	cmd := cobra.Command{
		Use:   "git <repositoryDir> <output>",
		Short: "Bundle files modified in a git repository",
		Args:  cobra.ExactArgs(2),
		Run: func(cmd *cobra.Command, args []string) {
			if err := runGitCmd(cmd, args, option); err != nil {
				printError(err, option.Verbose)
			}
		},
	}

	f := cmd.Flags()
	bindCmdOptionFlags(&option.BundleCmdOption, f)
	f.BoolVarP(&option.SkipConflicts, "skip-conflicts", "", false, "Skip files with merge conflicts")
	f.BoolVarP(&option.IncludeIgnored, "include-ignored", "", false, "")

	return &cmd
}

func runGitCmd(cobra *cobra.Command, args []string, option GitCmdOption) error {
	repoDir := args[0]
	gitFiles, err := getGitModifiedFiles(repoDir, option.IncludeIgnored)
	if err != nil {
		return ergo.Wrap(err, "get git modified files")
	}

	filterOption := FilterOption{
		ExcludePatterns: option.ExcludePatterns,
		Includes:        option.Includes,
		IncludeHidden:   option.IncludeHidden,
	}

	filtered := make([]GitFile, 0, len(gitFiles))
	for _, file := range gitFiles {
		picked, err := filterFilePath(repoDir, file.FilePath, filterOption)
		if err != nil {
			return ergo.Wrap(err, "filter file path")
		}
		if picked {
			filtered = append(filtered, file)
		}
	}

	filePathSet := make(map[string]struct{}, len(gitFiles))
	for _, gitFile := range gitFiles {
		filePathSet[gitFile.FilePath] = struct{}{}
	}

	var extraFilePaths []string
	if len(option.Includes) != 0 {
		repoDirFS := os.DirFS(repoDir)
		for _, pattern := range option.Includes {
			matchedPaths, err := doublestar.Glob(repoDirFS, pattern)
			if err != nil {
				return ergo.Wrap(err, "glob include pattern")
			}

			for _, matchedPath := range matchedPaths {
				if _, duplicated := filePathSet[matchedPath]; !duplicated {
					if isDir, err := checkIsDirectory(filepath.Join(repoDir, matchedPath)); err != nil {
						return ergo.Wrap(err, "")
					} else if !isDir {
						extraFilePaths = append(extraFilePaths, matchedPath)
						filePathSet[matchedPath] = struct{}{}
					}
				}
			}
		}
	}

	if len(filtered) == 0 {
		return nil
	}

	outputPath := args[len(args)-1]
	outputFile, err := prepareOutput(outputPath)
	if err != nil {
		return ergo.Wrap(err, "prepare output")
	}

	if err := writeGitStatusSummary(gitFiles, outputFile, option.SkipConflicts); err != nil {
		return ergo.Wrap(err, "write git status summary")
	}

	if option.WriteIndex {
		filePathList := make([]string, 0, len(filtered)+len(extraFilePaths))
		for _, gitFile := range filtered {
			filePathList = append(filePathList, gitFile.FilePath)
		}
		filePathList = append(filePathList, extraFilePaths...)

		if err := writeIndexOfFiles(filePathList, outputFile); err != nil {
			return ergo.Wrap(err, "write index of files")
		}
	}

	for _, gitFile := range filtered {
		if gitFile.IsDeleted() || (option.SkipConflicts && gitFile.IsConflicted()) {
			continue
		}
		filePath := filepath.Join(repoDir, gitFile.FilePath)
		if err := writeToFile(filePath, outputFile); err != nil {
			return ergo.Wrap(err, "write to file", slog.String("src", filePath), slog.String("dst", outputPath))
		}
	}

	for _, filePath := range extraFilePaths {
		srcFilePath := filepath.Join(repoDir, filePath)
		if err := writeToFile(srcFilePath, outputFile); err != nil {
			return ergo.Wrap(err, "write extra file to output")
		}
	}

	return nil
}

type GitFile struct {
	Status           string
	FilePath         string
	OriginalFilePath *string
}

func (g GitFile) IsConflicted() bool {
	return len(g.Status) == 2 && (g.Status[0] == 'U' || g.Status[1] == 'U')
}

func (g GitFile) IsDeleted() bool {
	return len(g.Status) == 2 && (g.Status[0] == 'D' || g.Status[1] == 'D')
}

func getGitModifiedFiles(dirPath string, includeIgnored bool) ([]GitFile, error) {
	gitCmdArgs := []string{"-C", dirPath, "status", "-uall", "--porcelain"}
	if includeIgnored {
		gitCmdArgs = append(gitCmdArgs, "--ignored")
	}
	cmd := exec.Command("git", gitCmdArgs...)

	var buf bytes.Buffer
	cmd.Stdout = &buf

	if err := cmd.Run(); err != nil {
		return nil, ergo.New("get status of git repository")
	}

	var gitFiles []GitFile

	scanner := bufio.NewScanner(&buf)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) < 4 {
			continue
		}

		status := line[:2]
		filePath := line[3:]

		gitFile := GitFile{
			Status:   status,
			FilePath: filePath,
		}

		if status[0] == 'R' || status[1] == 'R' {
			arrowIndex := strings.Index(filePath, " -> ")
			if arrowIndex != -1 {
				gitFile.FilePath = filePath[arrowIndex+len(" -> "):]
				gitFile.OriginalFilePath = new(filePath[:arrowIndex])
			}
		}

		gitFiles = append(gitFiles, gitFile)
	}

	return gitFiles, nil
}

func writeGitStatusSummary(gitFiles []GitFile, dst io.Writer, skipConflicts bool) error {
	sb := strings.Builder{}

	sb.WriteString("===== GIT STATUS SUMMARY =====\n")

	for _, gitFile := range gitFiles {
		if gitFile.OriginalFilePath == nil {
			sb.WriteString(fmt.Sprintf("%s %s", gitFile.Status, gitFile.FilePath))
		} else {
			sb.WriteString(fmt.Sprintf("%s %s -> %s", gitFile.Status, *gitFile.OriginalFilePath, gitFile.FilePath))
		}

		if skipConflicts && gitFile.IsConflicted() {
			sb.WriteString(" (Not include)")
		}
		sb.WriteByte('\n')
	}

	sb.WriteString("==============================\n\n")

	if _, err := io.WriteString(dst, sb.String()); err != nil {
		return ergo.Wrap(err, "write git status summary to output")
	}
	return nil
}

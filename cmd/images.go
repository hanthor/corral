package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/tuna-os/corral/pkg/catalog"
	"github.com/tuna-os/corral/pkg/kubevirt"
	"github.com/tuna-os/corral/pkg/types"
)

// imagesType filters `corral images`/`corral images search` to one catalog
// ("os", "bootc") or both ("all", the default) — see catalogSections.
var imagesType string

var imagesCmd = &cobra.Command{
	Use:   "images",
	Short: "Browse and pull the curated image catalog (OS images + bootc images)",
	Long: `List ready-to-boot images from both built-in catalogs:

  - OS images (cloud images, installer ISOs) — boot directly, no plugin needed:
      corral create myvm --image ubuntu

  - Bootc images (bootable containers: Fedora/CentOS bootc, Universal Blue,
    Bluefin/Aurora/Bazzite) — built into a VM disk by the bootc plugin:
      corral plugin install bootc
      corral bootc create myvm --image bluefin

--type os or --type bootc shows just one catalog.

Pull an OS image as a reusable golden template (clone-from-template):

  corral images pull ubuntu

For anything else, import a qcow2/raw disk image by URL or file:

  corral import myvm --source https://…/image.qcow2`,
	Example: `  corral images                     # both catalogs
  corral images --type bootc        # bootc images only
  corral images search gnome        # search both catalogs
  corral create myvm --image fedora
  corral bootc create myvm --image bluefin`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateImagesType(imagesType); err != nil {
			return err
		}
		if jsonOutput {
			return printCatalogJSON(imagesType, catalog.Images, catalog.BootcImages)
		}
		printCatalogSections(imagesType, catalog.Images, catalog.BootcImages)
		return nil
	},
}

// validateImagesType rejects anything but the three --type values catalog
// display/search understand, so a typo shows an error instead of silently
// falling through to "show both".
func validateImagesType(t string) error {
	switch t {
	case "", "os", "bootc":
		return nil
	default:
		return fmt.Errorf("--type must be \"os\" or \"bootc\" (got %q)", t)
	}
}

var imagesSearchCmd = &cobra.Command{
	Use:     "search [term]",
	Short:   "Search the image catalog (OS + bootc, unless --type narrows it)",
	Args:    cobra.MaximumNArgs(1),
	Example: `  corral images search gnome`,
	RunE: func(cmd *cobra.Command, args []string) error {
		if err := validateImagesType(imagesType); err != nil {
			return err
		}
		term := ""
		if len(args) == 1 {
			term = args[0]
		}
		osMatches, bootcMatches := searchCatalogs(term, imagesType)
		if len(osMatches) == 0 && len(bootcMatches) == 0 {
			return fmt.Errorf("no catalog image matches %q", term)
		}
		if jsonOutput {
			return printCatalogJSON(imagesType, osMatches, bootcMatches)
		}
		printCatalogSections(imagesType, osMatches, bootcMatches)
		return nil
	},
}

var imagesPullNamespace string

var imagesPullCmd = &cobra.Command{
	Use:   "pull <name>",
	Short: "Pull a catalog image as a golden VM template",
	Long: `Creates a stopped VM from the catalog image and marks it as a Corral
template. New VMs clone from it (disks included):

  corral images pull ubuntu
  corral template new tmpl-ubuntu dev1   # or clone in the web UI`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return pullTemplate(args[0], imagesPullNamespace)
	},
}

func printCatalog(images []catalog.Image) {
	fmt.Printf("%-26s %-10s %-14s %-24s %s\n", "NAME", "USER", "TYPE", "SOURCE", "DESCRIPTION")
	for _, img := range images {
		fmt.Printf("%-26s %-10s %-14s %-24s %s\n", img.Name, img.DefaultUser, img.Kind(), img.Source, img.Description)
	}
}

// searchCatalog filters by substring over name + description + source ("" = all).
func searchCatalog(term string) []catalog.Image {
	term = strings.ToLower(term)
	var out []catalog.Image
	for _, img := range catalog.Images {
		if term == "" ||
			strings.Contains(strings.ToLower(img.Name), term) ||
			strings.Contains(strings.ToLower(img.Description), term) ||
			strings.Contains(strings.ToLower(img.Source), term) {
			out = append(out, img)
		}
	}
	return out
}

// searchBootcCatalog mirrors searchCatalog for the bootc image catalog.
func searchBootcCatalog(term string) []catalog.BootcImage {
	term = strings.ToLower(term)
	var out []catalog.BootcImage
	for _, img := range catalog.BootcImages {
		if term == "" ||
			strings.Contains(strings.ToLower(img.Name), term) ||
			strings.Contains(strings.ToLower(img.Description), term) ||
			strings.Contains(strings.ToLower(img.Source), term) {
			out = append(out, img)
		}
	}
	return out
}

// searchCatalogs applies searchCatalog/searchBootcCatalog, narrowed by
// typeFilter ("os", "bootc", or "" / "all" for both).
func searchCatalogs(term, typeFilter string) (os []catalog.Image, bootc []catalog.BootcImage) {
	if typeFilter != "bootc" {
		os = searchCatalog(term)
	}
	if typeFilter != "os" {
		bootc = searchBootcCatalog(term)
	}
	return os, bootc
}

func printBootcCatalog(images []catalog.BootcImage) {
	fmt.Printf("%-26s %-24s %s\n", "NAME", "SOURCE", "DESCRIPTION")
	for _, img := range images {
		fmt.Printf("%-26s %-24s %s\n", img.Name, img.Source, img.Description)
	}
}

// printCatalogSections renders the OS and/or bootc catalogs per typeFilter
// ("os", "bootc", or "" / "all" for both), each clearly headed so `corral
// images` surfaces both without the bootc plugin needing to be discovered
// separately first.
func printCatalogJSON(typeFilter string, osImages []catalog.Image, bootcImages []catalog.BootcImage) error {
	if typeFilter == "os" {
		return printJSON(osImages)
	}
	if typeFilter == "bootc" {
		return printJSON(bootcImages)
	}
	return printJSON(struct {
		OS    []catalog.Image      `json:"os"`
		Bootc []catalog.BootcImage `json:"bootc"`
	}{OS: osImages, Bootc: bootcImages})
}

func printCatalogSections(typeFilter string, osImages []catalog.Image, bootcImages []catalog.BootcImage) {
	showOS := typeFilter != "bootc"
	showBootc := typeFilter != "os"

	if showOS {
		fmt.Println("OS images — corral create myvm --image <name>")
		printCatalog(osImages)
	}
	if showOS && showBootc {
		fmt.Println()
	}
	if showBootc {
		fmt.Println("Bootc images — corral bootc create myvm --image <name> (needs: corral plugin install bootc)")
		printBootcCatalog(bootcImages)
	}
}

// templateName is the VM name a pulled catalog image gets.
func templateName(image string) string { return "tmpl-" + image }

// pullTemplate creates a stopped VM from a catalog image and marks it as a
// template for clone-from-template.
func pullTemplate(image, ns string) error {
	img := catalog.Find(image)
	if img == nil {
		return fmt.Errorf("unknown image %q — see `corral images`", image)
	}
	if img.ISO != "" {
		return fmt.Errorf("%q is an installer ISO — it needs a manual install, so it can't be pulled as a template; create a VM with it instead: corral create myvm --image %s", image, image)
	}
	if ns == "" {
		ns = kubevirt.DefaultNamespace
	}
	name := templateName(image)
	opts := types.CreateOpts{
		Name:          name,
		Namespace:     ns,
		ContainerDisk: img.ContainerDisk,
		ImportURL:     img.URL,
		SSHPublicKey:  kubevirt.LoadSSHPublicKey(),
	}
	if err := kubevirt.CreateVM(opts); err != nil {
		return err
	}
	if err := kubevirt.NewClient(ns).MarkTemplate(name, true); err != nil {
		return fmt.Errorf("marking template: %w", err)
	}
	fmt.Printf("template %q ready — clone with: corral template new %s <vm>\n", name, name)
	return nil
}

func init() {
	imagesPullCmd.Flags().StringVarP(&imagesPullNamespace, "namespace", "n", "", "Namespace (default corral)")
	imagesCmd.PersistentFlags().StringVar(&imagesType, "type", "", "Filter to one catalog: os, bootc (default: both)")
	imagesCmd.AddCommand(imagesSearchCmd, imagesPullCmd)
	rootCmd.AddCommand(imagesCmd)
}

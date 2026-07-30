package windows

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// RandomPassword generates a password meeting Windows' default
// complexity policy (3 of 4 character classes, min length) — corral's
// existing randomPassword() is lowercase+digits only, which Windows'
// local security policy rejects even for a scripted AdministratorPassword
// in an unattend file.
func RandomPassword() string {
	const (
		lower = "abcdefghijklmnopqrstuvwxyz"
		upper = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
		digit = "0123456789"
		// XML-safe (no & < > " ' which would need escaping inside the
		// unattend file's <Value> elements).
		special = "!@#$%^*-_="
	)
	pick := func(charset string, n int) string {
		out := make([]byte, n)
		for i := range out {
			idx, _ := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
			out[i] = charset[idx.Int64()]
		}
		return string(out)
	}
	// Guarantee at least one of each class, then pad, then shuffle.
	pw := []byte(pick(lower, 3) + pick(upper, 3) + pick(digit, 3) + pick(special, 3))
	for i := len(pw) - 1; i > 0; i-- {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(i+1)))
		pw[i], pw[j.Int64()] = pw[j.Int64()], pw[i]
	}
	return string(pw)
}

// Windows autounattend support. Windows Setup natively reads an
// autounattend.xml from the root of any attached removable media (CD-ROM
// included) and, when present, skips every interactive prompt — language,
// disk partitioning, EULA, account creation, OOBE. This is the standard
// Microsoft mechanism real automation tools use (dockur/windows's
// install.sh injects the same file into its ISO) — not blind VNC keyboard
// scripting, which is what corral tried first and found fragile: UEFI
// firmware menus don't expose stable coordinates/focus state to script
// against reliably.
//
// KubeVirt has no "burn an arbitrary file onto a CD-ROM" primitive, but it
// does have exactly the building block needed: a `configMap` volume
// presented as a `cdrom` device gets packaged into a real ISO9660 image by
// KubeVirt itself, mounted read-only in the guest. One ConfigMap key,
// named autounattend.xml, is all Setup needs to find at that CD-ROM's root.

// AutounattendXML renders a minimal, working answer file: wipe the boot
// disk and create a standard UEFI/GPT layout (EFI system partition, MSR,
// Windows partition — the first virtio disk KubeVirt hands Setup),
// install image index 1 (the first edition on the install media — most
// consumer/eval ISOs ship one), skip every OOBE prompt, create a local
// Administrator account with adminPassword, and enable RDP so the VM is
// reachable the moment installation finishes with no further manual
// setup. computerName becomes the guest's hostname.
//
// This is deliberately not a complete unattend-schema reference
// implementation (no domain join, no driver injection beyond what's
// already in the boot image, no locale beyond en-US) — it's scoped to
// "get a working, reachable Windows desktop with zero interactive clicks,"
// matching corral's own tracer-bullet scoping elsewhere.
func AutounattendXML(computerName, adminPassword string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="utf-8"?>
<unattend xmlns="urn:schemas-microsoft-com:unattend">
  <settings pass="windowsPE">
    <component name="Microsoft-Windows-International-Core-WinPE" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <SetupUILanguage><UILanguage>en-US</UILanguage></SetupUILanguage>
      <InputLocale>en-US</InputLocale>
      <SystemLocale>en-US</SystemLocale>
      <UILanguage>en-US</UILanguage>
      <UserLocale>en-US</UserLocale>
    </component>
    <component name="Microsoft-Windows-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <DiskConfiguration>
        <Disk wcm:action="add">
          <DiskID>0</DiskID>
          <WillWipeDisk>true</WillWipeDisk>
          <CreatePartitions>
            <CreatePartition wcm:action="add">
              <Order>1</Order>
              <Type>EFI</Type>
              <Size>260</Size>
            </CreatePartition>
            <CreatePartition wcm:action="add">
              <Order>2</Order>
              <Type>MSR</Type>
              <Size>16</Size>
            </CreatePartition>
            <CreatePartition wcm:action="add">
              <Order>3</Order>
              <Type>Primary</Type>
              <Extend>true</Extend>
            </CreatePartition>
          </CreatePartitions>
          <ModifyPartitions>
            <ModifyPartition wcm:action="add">
              <Order>1</Order>
              <PartitionID>1</PartitionID>
              <Format>FAT32</Format>
              <Label>System</Label>
            </ModifyPartition>
            <ModifyPartition wcm:action="add">
              <Order>2</Order>
              <PartitionID>2</PartitionID>
            </ModifyPartition>
            <ModifyPartition wcm:action="add">
              <Order>3</Order>
              <PartitionID>3</PartitionID>
              <Format>NTFS</Format>
              <Label>Windows</Label>
              <Letter>C</Letter>
            </ModifyPartition>
          </ModifyPartitions>
        </Disk>
        <WillShowUI>OnError</WillShowUI>
      </DiskConfiguration>
      <ImageInstall>
        <OSImage>
          <InstallFrom>
            <MetaData wcm:action="add">
              <Key>/IMAGE/INDEX</Key>
              <Value>1</Value>
            </MetaData>
          </InstallFrom>
          <InstallTo>
            <DiskID>0</DiskID>
            <PartitionID>3</PartitionID>
          </InstallTo>
        </OSImage>
      </ImageInstall>
      <UserData>
        <AcceptEula>true</AcceptEula>
      </UserData>
    </component>
  </settings>
  <settings pass="specialize">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <ComputerName>%s</ComputerName>
    </component>
    <component name="Microsoft-Windows-TerminalServices-LocalSessionManager" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <fDenyTSConnections>false</fDenyTSConnections>
    </component>
    <component name="Networking-MPSSVC-Svc" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <FirewallGroups>
        <FirewallGroup wcm:action="add" wcm:keyValue="RemoteDesktop">
          <Active>true</Active>
          <Profile>all</Profile>
          <Group>Remote Desktop</Group>
        </FirewallGroup>
      </FirewallGroups>
    </component>
  </settings>
  <settings pass="oobeSystem">
    <component name="Microsoft-Windows-Shell-Setup" processorArchitecture="amd64" publicKeyToken="31bf3856ad364e35" language="neutral" versionScope="nonSxS">
      <OOBE>
        <HideEULAPage>true</HideEULAPage>
        <HideOEMRegistrationScreen>true</HideOEMRegistrationScreen>
        <HideOnlineAccountScreens>true</HideOnlineAccountScreens>
        <HideWirelessSetupInOOBE>true</HideWirelessSetupInOOBE>
        <ProtectYourPC>3</ProtectYourPC>
        <NetworkLocation>Work</NetworkLocation>
      </OOBE>
      <UserAccounts>
        <AdministratorPassword>
          <Value>%s</Value>
          <PlainText>true</PlainText>
        </AdministratorPassword>
      </UserAccounts>
      <AutoLogon>
        <Password>
          <Value>%s</Value>
          <PlainText>true</PlainText>
        </Password>
        <Enabled>true</Enabled>
        <Username>Administrator</Username>
        <LogonCount>1</LogonCount>
      </AutoLogon>
      <TimeZone>UTC</TimeZone>
    </component>
  </settings>
</unattend>
`, computerName, adminPassword, adminPassword)
}

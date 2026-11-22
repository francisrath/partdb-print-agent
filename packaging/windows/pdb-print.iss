; setup.exe for pdb-print (built by scripts/windows-setup.sh with Inno Setup under Wine).
; It installs nothing itself: it runs `pdb-print.exe install`, which does the per-user install, or with
; "for all users" (/ALLUSERS) the Windows service. Uninstall is pdb-print's own (Settings > Apps).
;   setup.exe                                   asks: for me or for all users
;   setup.exe /VERYSILENT /ALLUSERS /NOSELFUPDATE   service, no self-update (as administrator)

#ifndef Version
  #error pass /DVersion=x.y.z
#endif

[Setup]
AppId=pdb-print
AppName=pdb-print
AppVersion={#Version}
AppPublisher=Francis Rath
AppPublisherURL=https://github.com/francisrath/pdb-print
PrivilegesRequired=lowest
PrivilegesRequiredOverridesAllowed=commandline dialog
Uninstallable=no
CreateAppDir=no
DisableProgramGroupPage=yes
DisableReadyPage=yes
MinVersion=10.0
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
WizardStyle=modern
SetupIconFile=pdb-print.ico
OutputDir=.
OutputBaseFilename=pdb-print_{#Version}_windows_amd64_setup
Compression=lzma2/max
SolidCompression=yes

[Files]
; extracted and run by PrepareToInstall: `install` copies both exes to their place
Source: "pdb-print.exe"; DestDir: "{tmp}"; Flags: dontcopy
Source: "pdb-printw.exe"; DestDir: "{tmp}"; Flags: dontcopy

[Run]
; as the user who started setup, not as the administrator it may have become
Filename: "http://127.0.0.1:8631"; Description: "Open pdb-print"; Flags: postinstall shellexec runasoriginaluser skipifsilent nowait

[Code]
function HasParam(const Name: String): Boolean;
var
  I: Integer;
begin
  Result := False;
  for I := 1 to ParamCount do
    if CompareText(ParamStr(I), Name) = 0 then
      Result := True;
end;

// pdb-print.exe install does the work; its error becomes setup's (exit code 7 when silent)
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Args, Msg: String;
  Code, I: Integer;
  Output: TExecOutput;
begin
  Result := '';
  ExtractTemporaryFile('pdb-print.exe');
  ExtractTemporaryFile('pdb-printw.exe');
  Args := 'install --quiet'; // the finish page opens the browser, as the user
  if IsAdminInstallMode then
    Args := Args + ' --service';
  if HasParam('/NOSELFUPDATE') then
    Args := Args + ' --no-self-update';
  Log('pdb-print.exe ' + Args);
  if not ExecAndCaptureOutput(ExpandConstant('{tmp}\pdb-print.exe'), Args, ExpandConstant('{tmp}'), SW_HIDE,
    ewWaitUntilTerminated, Code, Output) then
  begin
    Result := 'pdb-print.exe could not be started: ' + SysErrorMessage(Code);
    Exit;
  end;
  for I := 0 to GetArrayLength(Output.StdOut) - 1 do
    Log(Output.StdOut[I]);
  Msg := '';
  for I := 0 to GetArrayLength(Output.StdErr) - 1 do
  begin
    Log(Output.StdErr[I]);
    Msg := Msg + Output.StdErr[I] + #13#10;
  end;
  if Code <> 0 then
    Result := 'pdb-print could not be installed.' + #13#10#13#10 + Trim(Msg);
end;

procedure CurPageChanged(CurPageID: Integer);
begin
  if CurPageID <> wpFinished then
    Exit;
  if IsAdminInstallMode then
    WizardForm.FinishedLabel.Caption := 'pdb-print runs as a Windows service for everyone on this PC.' + #13#10#13#10 +
      'To set it up in the browser, run this in a Command Prompt as administrator and type the code it shows:' + #13#10#13#10 +
      '"' + ExpandConstant('{commonpf64}') + '\pdb-print\pdb-print.exe" pair'
  else
    WizardForm.FinishedLabel.Caption := 'pdb-print is installed and starts when you log in.';
end;

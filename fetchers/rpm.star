"""
Package Fetcher for RPM Repositories.
"""

def get_rpm_name(ctx, ent, arch):
    if ent["Name"] == "filesystem":
        return None
    elif " if redhat-rpm-config)" in ent["Name"]:
        return None
    elif " if clang)" in ent["Name"]:
        return None
    elif " if gcc)" in ent["Name"]:
        return None

    ver = ent["Ver"]
    flags = ent["Flags"]
    if flags != "":
        if flags == "EQ":
            pass
        elif flags == "GE":
            ver = ">" + ver
        elif flags == "LE":
            ver = "<" + ver
        elif flags == "GT":
            ver = ">" + ver
        elif flags == "LT":
            ver = "<" + ver
        else:
            print("flags unhandled", flags, ver)

    return ctx.name(
        name = ent["Name"],
        version = ver,
        architecture = arch,
    )

def fetch_rpm_repostiory(ctx, url):
    repomd_url = url + "repodata/repomd.xml"

    repomd = parse_xml(fetch_http(repomd_url).read())

    primary = [
        element
        for element in repomd["repomd"]["data"]
        if element["-type"] == "primary"
    ][0]

    primary_url = primary["location"]["-href"]
    primary_size = 0
    if "size" in primary:
        primary_size = int(primary["size"])

    primary = fetch_http(
        url + primary_url,
        expected_size = primary_size,
    ).read_compressed(primary_url).read_rpm_xml()

    for ent in primary:
        arch = ent["Arch"]
        if arch == "noarch":
            arch = "any"

        pkg = ctx.add_package(ctx.name(
            name = ent["Name"],
            version = ent["Version"]["Ver"],
            architecture = arch,
        ))

        pkg.set_raw(json.encode(ent))

        if "Description" in ent:
            pkg.set_description(ent["Description"])
        pkg.set_license(ent["Format"]["License"])
        pkg.add_source(kind = "rpm", url = url + ent["Location"]["Href"])

        if ent["Format"]["Requires"]["Entry"] != None:
            for require in ent["Format"]["Requires"]["Entry"]:
                pkg.add_dependency(get_rpm_name(ctx, require, arch))

        if ent["Format"]["Provides"]["Entry"] != None:
            for provide in ent["Format"]["Provides"]["Entry"]:
                pkg.add_alias(get_rpm_name(ctx, provide, arch))

        if ent["Format"]["File"] != None:
            for ent in ent["Format"]["File"]:
                pkg.add_alias(ctx.name(
                    name = ent["Text"],
                    architecture = arch,
                ))

def get_rpm_contents(ctx, pkg, url):
    fs = filesystem()

    rpm = parse_rpm(fetch_http(url))

    metadata = json.decode(rpm.metadata)

    fs[".pkg/rpm/metadata/" + pkg.name + ".json"] = rpm.metadata

    if metadata["PreInstallScript"] != "":
        fs[".pkg/rpm/pre-install/" + pkg.name + ".sh"] = file(metadata["PreInstallScript"], executable = True)
    if metadata["PostInstallScript"] != "":
        fs[".pkg/rpm/post-install/" + pkg.name + ".sh"] = file(metadata["PostInstallScript"], executable = True)

    if rpm.payload_compression == "zstd":
        ark = rpm.payload.read_archive(".cpio.zst")

        for f in ark:
            name = f.name.removeprefix("./")
            if name.endswith("/"):
                continue  # ignore directoriess
            fs[name] = f

        return ctx.archive(fs)
    else:
        return error("payload compression not implemented: " + rpm.payload_compression)

register_content_fetcher(
    "rpm",
    get_rpm_contents,
    (),
)

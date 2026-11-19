using System.Reflection;
using System.Runtime.InteropServices;

// Prints the public API of the listed types of Microsoft.macOS.dll (the reference assembly), read as metadata only.
// args: <Microsoft.macOS.dll> <types.txt>   (types.txt: one full type name per line; a trailing * matches a prefix)
var dll = args[0];
var patterns = File.ReadAllLines(args[1]).Select(l => l.Trim()).Where(l => l.Length > 0 && !l.StartsWith('#')).ToArray();
var paths = Directory.GetFiles(RuntimeEnvironment.GetRuntimeDirectory(), "*.dll").Append(dll);
using var context = new MetadataLoadContext(new PathAssemblyResolver(paths));
var assembly = context.LoadFromAssemblyPath(dll);
Type[] types;
try { types = assembly.GetTypes(); }
catch (ReflectionTypeLoadException e) { types = e.Types.Where(t => t != null).ToArray()!; }
Console.WriteLine($"# {assembly.FullName} ({types.Length} types)");
const BindingFlags Flags = BindingFlags.Public | BindingFlags.Instance | BindingFlags.Static | BindingFlags.DeclaredOnly;
foreach (var pattern in patterns)
{
    var matched = types.Where(t => t.IsPublic || t.IsNestedPublic)
        .Where(t => pattern.EndsWith('*') ? (t.FullName ?? "").StartsWith(pattern[..^1], StringComparison.Ordinal) : t.FullName == pattern)
        .OrderBy(t => t.FullName).ToList();
    if (matched.Count == 0) { Console.WriteLine($"\n## {pattern}: NOT FOUND"); continue; }
    foreach (var t in matched)
    {
        var bases = new List<string>();
        if (t.BaseType != null) bases.Add(Name(t.BaseType));
        bases.AddRange(t.GetInterfaces().Select(Name));
        Console.WriteLine($"\n## {Kind(t)} {t.FullName} : {string.Join(", ", bases)}");
        if (t.IsEnum)
        {
            Console.WriteLine("  " + string.Join(", ", t.GetFields(BindingFlags.Public | BindingFlags.Static).Select(f => $"{f.Name}={f.GetRawConstantValue()}")));
            continue;
        }
        foreach (var m in t.GetMembers(Flags).OrderBy(m => m.MemberType.ToString()).ThenBy(m => m.Name))
        {
            var line = Format(m);
            if (line != null) Console.WriteLine("  " + line);
        }
    }
}

static string Kind(Type t) => t.IsEnum ? "enum" : t.IsInterface ? "interface" : t.IsValueType ? "struct" : typeof(Delegate).Name == t.BaseType?.Name || t.BaseType?.Name == "MulticastDelegate" ? "delegate" : t.IsAbstract && t.IsSealed ? "static class" : "class";

static string Name(Type t)
{
    if (t.IsByRef) return "ref " + Name(t.GetElementType()!);
    if (t.IsArray) return Name(t.GetElementType()!) + "[]";
    if (t.IsPointer) return Name(t.GetElementType()!) + "*";
    if (t.IsGenericType)
    {
        var name = t.Name.Contains('`') ? t.Name[..t.Name.IndexOf('`')] : t.Name;
        var arguments = string.Join(",", t.GetGenericArguments().Select(Name));
        return name == "Nullable" ? arguments + "?" : $"{name}<{arguments}>";
    }
    return t.FullName switch
    {
        "System.Void" => "void", "System.Boolean" => "bool", "System.String" => "string", "System.Int32" => "int", "System.Int64" => "long",
        "System.UInt32" => "uint", "System.UInt64" => "ulong", "System.Double" => "double", "System.Single" => "float", "System.Object" => "object",
        "System.IntPtr" => "nint", "System.UIntPtr" => "nuint", "System.Byte" => "byte", "System.Int16" => "short", "System.UInt16" => "ushort",
        _ => t.IsNested ? Name(t.DeclaringType!) + "." + t.Name : t.Name,
    };
}

static string Export(MemberInfo m)
{
    try
    {
        foreach (var a in m.GetCustomAttributesData())
        {
            if (a.AttributeType.Name == "ExportAttribute" && a.ConstructorArguments.Count > 0)
                return $"  [Export(\"{a.ConstructorArguments[0].Value}\")]";
            if (a.AttributeType.Name == "ObsoleteAttribute")
                return "  [Obsolete]";
        }
    }
    catch { }
    return "";
}

static string Params(MethodBase m) => string.Join(", ", m.GetParameters().Select(p => (p.IsOut ? "out " : "") + Name(p.ParameterType).Replace("ref ", p.IsOut ? "" : "ref ") + " " + p.Name + (p.IsOptional ? " = default" : "")));

static string? Format(MemberInfo m)
{
    switch (m)
    {
        case ConstructorInfo c:
            return $".ctor({Params(c)}){Export(c)}";
        case MethodInfo method when !method.IsSpecialName:
            return $"{(method.IsStatic ? "static " : method.IsAbstract ? "abstract " : method.IsVirtual ? "virtual " : "")}{Name(method.ReturnType)} {method.Name}{(method.IsGenericMethod ? "<>" : "")}({Params(method)}){Export(method)}";
        case PropertyInfo p:
            var accessor = p.GetMethod ?? p.SetMethod;
            return $"{(accessor?.IsStatic == true ? "static " : "")}{Name(p.PropertyType)} {p.Name} {{ {(p.GetMethod?.IsPublic == true ? "get; " : "")}{(p.SetMethod?.IsPublic == true ? "set; " : "")}}}{(accessor != null ? Export(accessor) : "")}";
        case EventInfo e:
            return $"event {Name(e.EventHandlerType!)} {e.Name}";
        case FieldInfo f:
            return $"{(f.IsStatic ? "static " : "")}{Name(f.FieldType)} {f.Name}{(f.IsLiteral ? " = " + f.GetRawConstantValue() : "")}";
        default:
            return null;
    }
}
